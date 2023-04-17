// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"golang.org/x/sync/singleflight"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"infra/build/siso/o11y/clog"
	"infra/build/siso/o11y/iometrics"
	"infra/build/siso/o11y/trace"
	"infra/build/siso/reapi/digest"
)

// LocalCache implements CacheStore interface with local files.
type LocalCache struct {
	dir string

	singleflight singleflight.Group
	m            *iometrics.IOMetrics
}

// NewLocalCache returns new local cache.
func NewLocalCache(dir string) (*LocalCache, error) {
	if dir == "" {
		return nil, errors.New("local cache is not configured")
	}
	return &LocalCache{
		dir: dir,
		m:   iometrics.New("local-cache"),
	}, nil
}

// IOMetrics returns io metrics of the local cache.
func (c *LocalCache) IOMetrics() *iometrics.IOMetrics {
	if c == nil {
		return nil
	}
	return c.m
}

func (c *LocalCache) actionCacheFilename(d digest.Digest) string {
	name := fmt.Sprintf("%s-%d", d.Hash, d.SizeBytes)
	return filepath.Join(c.dir, "actions", name[:2], name[2:])
}

func (c *LocalCache) contentCacheFilename(d digest.Digest) string {
	name := fmt.Sprintf("%s-%d.gz", d.Hash, d.SizeBytes)
	return filepath.Join(c.dir, "contents", name[:2], name[2:])
}

// GetActionResult gets the action result of the action identified by the digest.
func (c *LocalCache) GetActionResult(ctx context.Context, d digest.Digest) (*rpb.ActionResult, error) {
	if c == nil {
		return nil, status.Error(codes.NotFound, "cache is not configured")
	}
	fname := c.actionCacheFilename(d)
	b, err := os.ReadFile(fname)
	c.m.ReadDone(len(b), err)
	if errors.Is(err, os.ErrNotExist) {
		return nil, status.Errorf(codes.NotFound, "not found %s: %v", fname, err)
	}
	if err != nil {
		return nil, err
	}
	result := &rpb.ActionResult{}
	err = proto.Unmarshal(b, result)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal %s: %w", fname, err)
	}
	return result, nil
}

// GetContent returns content of the fname identified by the digest.
func (c *LocalCache) GetContent(ctx context.Context, d digest.Digest, _ string) ([]byte, error) {
	_, span := trace.NewSpan(ctx, "cache-get-content")
	defer span.Close(nil)
	cname := c.contentCacheFilename(d)
	r, err := os.Open(cname)
	if err != nil {
		c.m.ReadDone(0, err)
		return nil, err
	}
	defer r.Close()
	gr, err := gzip.NewReader(r)
	if err != nil {
		c.m.ReadDone(0, err)
		return nil, err
	}
	defer gr.Close()
	buf, err := io.ReadAll(gr)
	// TODO(b/274060507): local cache metric: iometrics uses compressed size or uncompressed size?
	c.m.ReadDone(len(buf), err)
	return buf, err
}

// SetContent sets content of fname identified by the digest.
func (c *LocalCache) SetContent(ctx context.Context, d digest.Digest, fname string, buf []byte) error {
	cname := c.contentCacheFilename(d)
	_, err := os.Stat(cname)
	c.m.OpsDone(err)
	if err == nil {
		return nil
	}
	err = os.MkdirAll(filepath.Dir(cname), 0755)
	c.m.OpsDone(err)
	if err != nil {
		return err
	}
	_, err, shared := c.singleflight.Do(cname, func() (any, error) {
		w, err := os.Create(cname)
		if err != nil {
			c.m.WriteDone(0, err)
			return nil, err
		}
		gw := gzip.NewWriter(w)
		_, err = gw.Write(buf)
		if err != nil {
			c.m.WriteDone(0, err)
			w.Close()
			return nil, err
		}
		err = gw.Close()
		if err != nil {
			c.m.WriteDone(0, err)
			w.Close()
			return nil, err
		}
		err = w.Close()
		// TODO(b/274060507): local cache metric: iometrics uses compressed size or uncompressed size?
		c.m.WriteDone(len(buf), err)
		return nil, err
	})
	clog.Infof(ctx, "write cache content %s for %s shared:%t: %v", d, fname, shared, err)
	return err
}

// HasContent checks whether content of the digest exists in the local cache.
func (c *LocalCache) HasContent(ctx context.Context, d digest.Digest) bool {
	cname := c.contentCacheFilename(d)
	_, err := os.Stat(cname)
	c.m.OpsDone(err)
	return err == nil
}

// DigestData returns digest data for fname identified by the digest.
func (c *LocalCache) DigestData(d digest.Digest, fname string) digest.Data {
	return digest.NewData(dataSource{c: c, d: d, fname: fname, m: c.IOMetrics()}, d)
}

type dataReadCloser struct {
	io.ReadCloser
	f io.Closer
	m *iometrics.IOMetrics
	n int
}

func (r *dataReadCloser) Read(buf []byte) (int, error) {
	n, err := r.ReadCloser.Read(buf)
	r.n += n
	return n, err
}

func (r *dataReadCloser) Close() error {
	err := r.ReadCloser.Close()
	if r.f != nil {
		cerr := r.f.Close()
		if err == nil {
			err = cerr
		}
	}
	r.m.ReadDone(r.n, err)
	return err
}

type dataSource struct {
	c     *LocalCache
	d     digest.Digest
	fname string
	m     *iometrics.IOMetrics
}

func (s dataSource) Open(ctx context.Context) (io.ReadCloser, error) {
	if s.d.SizeBytes == 0 {
		return io.NopCloser(bytes.NewReader(nil)), nil
	}
	if s.c == nil || s.c.dir == "" {
		return nil, errors.New("cache is not configured")
	}
	name := fmt.Sprintf("%s-%d.gz", s.d.Hash, s.d.SizeBytes)
	cname := filepath.Join(s.c.dir, "contents", name[:2], name[2:])
	r, err := os.Open(cname)
	if err != nil {
		var err2 error
		r, err2 = os.Open(s.fname)
		if err2 != nil {
			clog.Warningf(ctx, "failed to open cached-digest data %s for %s: %v %v", s.d, s.fname, err, err2)
			s.m.ReadDone(0, err)
			return nil, err
		}
		clog.Infof(ctx, "use %s (failed to open cached-digest data %s: %v)", s.fname, s.d, err)
		return &dataReadCloser{ReadCloser: r, m: s.m}, nil
	}
	gr, err := gzip.NewReader(r)
	if err != nil {
		r.Close()
		clog.Warningf(ctx, "failed to gunzip cached-digest data %s for %s: %v", s.d, s.fname, err)
		s.m.ReadDone(0, err)
		return nil, err
	}
	return &dataReadCloser{ReadCloser: gr, f: r, m: s.m}, nil
}

func (s dataSource) String() string {
	return fmt.Sprintf("cache %s for %s", s.d, s.fname)
}
