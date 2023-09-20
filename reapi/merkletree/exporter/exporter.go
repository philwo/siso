// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package exporter is an exporter of directory tree from RBE-CAS.
package exporter

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/proto"

	"infra/build/siso/o11y/clog"
	"infra/build/siso/reapi/digest"
	"infra/build/siso/sync/semaphore"
)

// Client is an interface to access CAS.
type Client interface {
	Get(context.Context, digest.Digest, string) ([]byte, error)
}

// Exporter is an exporter.
type Exporter struct {
	client Client
	eg     errgroup.Group
	sema   *semaphore.Semaphore
}

// New creates new exporter.
func New(client Client) *Exporter {
	return &Exporter{
		client: client,
		sema:   semaphore.New("exporter", runtime.NumCPU()),
	}
}

// Export exports directory identified by the digest to the dir recursively.
func (e *Exporter) Export(ctx context.Context, dir string, d digest.Digest) error {
	e.eg.Go(func() error {
		return e.sema.Do(ctx, func(ctx context.Context) error {
			return e.exportDir(ctx, dir, d)
		})
	})
	return e.eg.Wait()
}

func (e *Exporter) exportDir(ctx context.Context, dir string, d digest.Digest) error {
	clog.Infof(ctx, "export dir: %s %s", dir, d)
	err := os.MkdirAll(dir, 0755)
	if err != nil {
		return err
	}
	b, err := e.client.Get(ctx, d, dir)
	if err != nil {
		return err
	}
	curdir := &rpb.Directory{}
	err = proto.Unmarshal(b, curdir)
	if err != nil {
		return fmt.Errorf("failed to unmarshal dir for %s from %s: %v", dir, d, err)
	}
	for _, f := range curdir.Files {
		f := f
		e.eg.Go(func() error {
			return e.sema.Do(ctx, func(ctx context.Context) error {
				return e.exportFile(ctx, filepath.Join(dir, f.Name), digest.FromProto(f.Digest), f.IsExecutable)
			})
		})
	}
	for _, subdir := range curdir.Directories {
		subdir := subdir
		e.eg.Go(func() error {
			return e.sema.Do(ctx, func(ctx context.Context) error {
				return e.exportDir(ctx, filepath.Join(dir, subdir.Name), digest.FromProto(subdir.Digest))
			})
		})
	}
	for _, s := range curdir.Symlinks {
		fname := filepath.Join(dir, s.Name)
		clog.Infof(ctx, "symlink %s -> %s", fname, s.Target)
		err := os.Symlink(s.Target, fname)
		if err != nil {
			return err
		}
	}
	return nil
}

func (e *Exporter) exportFile(ctx context.Context, fname string, d digest.Digest, isExecutable bool) error {
	clog.Infof(ctx, "file:%s %s x:%t", fname, d, isExecutable)
	b, err := e.client.Get(ctx, d, fname)
	if err != nil {
		return err
	}
	mode := os.FileMode(0644)
	if isExecutable {
		mode = os.FileMode(0755)
	}
	err = os.WriteFile(fname, b, mode)
	if err != nil {
		return err
	}
	return os.Chmod(fname, mode)
}
