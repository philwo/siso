// Copyright 2023 The Chromium Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"time"

	"cloud.google.com/go/storage"
	"golang.org/x/sync/errgroup"
)

// TODO(b/267576561): support Cloud Trace integration.

// SharedDepsLog is a shared deps log on cloud storage bucket.
type SharedDepsLog struct {
	Bucket *storage.BucketHandle
}

type depsLogEntry struct {
	Deps []string `json:"deps"`
}

// Get gets a deps log from Google Cloud Storage.
func (d SharedDepsLog) Get(ctx context.Context, output string, cmdhash []byte) ([]string, time.Time, error) {
	if d.Bucket == nil {
		return nil, time.Time{}, errors.New("not found")
	}
	obj := d.Bucket.Object(path.Join(filepath.ToSlash(output), hex.EncodeToString(cmdhash)))
	rd, err := obj.NewReader(ctx)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer rd.Close()
	buf, err := io.ReadAll(rd)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("read err %s: %w", obj.ObjectName(), err)
	}
	var ent depsLogEntry
	err = json.Unmarshal(buf, &ent)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("unmarshal err %s: %w", obj.ObjectName(), err)
	}
	return ent.Deps, rd.Attrs.LastModified, nil
}

// Record records a deps log deps on Google Cloud Storage.
func (d SharedDepsLog) Record(ctx context.Context, output string, cmdhash []byte, deps []string) (updated bool, err error) {
	if d.Bucket == nil {
		return false, nil
	}
	ent := depsLogEntry{
		Deps: deps,
	}
	obj := d.Bucket.Object(path.Join(filepath.ToSlash(output), hex.EncodeToString(cmdhash)))
	buf, err := json.Marshal(ent)
	if err != nil {
		return false, fmt.Errorf("marshal %s: %w", obj.ObjectName(), err)
	}
	// Upload without checking the content of the object because it may take longer to download.
	wr := obj.NewWriter(ctx)
	defer func() {
		if e := wr.Close(); e != nil {
			updated = false
			// Do not overwrite an error from Write() below.
			if err == nil {
				err = e
			}
		}
	}()
	for len(buf) > 0 {
		n, err := wr.Write(buf)
		if err != nil {
			updated = false
			return updated, fmt.Errorf("write %s: %w", obj.ObjectName(), err)
		}
		buf = buf[n:]
	}
	updated = true
	return updated, nil
}

// recordDepsLog records deps for output with cmdhash to local and shared.
func (b *Builder) recordDepsLog(ctx context.Context, output string, cmdhash []byte, t time.Time, deps []string) (bool, error) {
	g, ctx := errgroup.WithContext(ctx)

	// updated flags
	var lu, su bool
	g.Go(func() (err error) {
		lu, err = b.stepDefs.RecordDepsLog(ctx, output, t, deps)
		return err
	})
	if b.sharedDepsLog.Bucket != nil {
		g.Go(func() (err error) {
			su, err = b.sharedDepsLog.Record(ctx, output, cmdhash, deps)
			return err
		})
	}
	err := g.Wait()
	return lu || su, err
}
