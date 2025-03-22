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
	"strconv"
	"time"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/golang/glog"
	"golang.org/x/sync/singleflight"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"go.chromium.org/infra/build/siso/reapi/digest"
	"go.chromium.org/infra/build/siso/ui"
)

// LocalCache implements CacheStore interface with local files.
type LocalCache struct {
	dir string

	singleflight singleflight.Group
	timestamp    time.Time
}

// There is an upper bound on lifespan of 2 * TTL, since something that's
// expired may not actually be picked up again until the next garbage
// collection, which may not be for TTL.
const localCacheTTL = 7 * 24 * time.Hour

// NewLocalCache returns new local cache.
func NewLocalCache(dir string) (*LocalCache, error) {
	if dir == "" {
		return nil, errors.New("local cache is not configured")
	}
	return &LocalCache{
		dir: dir,
		// Use the same timestamp throughout the build. Makes things simpler.
		timestamp: time.Now(),
	}, nil
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

	if err := os.Chtimes(fname, c.timestamp, c.timestamp); err != nil {
		glog.Warningf("Failed to update mtime for %s: %v", fname, err)
	}
	return result, nil
}

// SetActionResult sets the action result of the action identified by the digest.
// If a failing action is provided, caching will be skipped.
func (c *LocalCache) SetActionResult(ctx context.Context, d digest.Digest, ar *rpb.ActionResult) error {
	// Don't cache failing actions, as RBE won't either.
	if c == nil || ar.ExitCode != 0 {
		return nil
	}
	b, err := proto.Marshal(ar)
	if err != nil {
		return nil
	}

	fname := c.actionCacheFilename(d)
	_, err, _ = c.singleflight.Do(fname, func() (any, error) {
		err := os.MkdirAll(filepath.Dir(fname), 0755)
		if err != nil {
			return nil, err
		}
		// Write to a temporary file first before renaming to perform an atomic
		// write.
		tmp := fname + ".tmp"
		err = os.WriteFile(tmp, b, 0644)
		if err != nil {
			os.Remove(tmp)
			return nil, err
		}
		err = os.Rename(tmp, fname)
		if err != nil {
			os.Remove(tmp)
			return nil, err
		}
		return nil, nil
	})
	return err
}

// GetContent returns content of the fname identified by the digest.
func (c *LocalCache) GetContent(ctx context.Context, d digest.Digest, _ string) ([]byte, error) {
	cname := c.contentCacheFilename(d)
	r, err := os.Open(cname)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	gr, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	defer gr.Close()
	buf, err := io.ReadAll(gr)
	if err == nil {
		if err := os.Chtimes(cname, c.timestamp, c.timestamp); err != nil {
			glog.Warningf("Failed to update mtime for %s: %v", cname, err)
		}
	}
	return buf, err
}

// SetContent sets content of fname identified by the digest.
func (c *LocalCache) SetContent(ctx context.Context, d digest.Digest, fname string, buf []byte) error {
	cname := c.contentCacheFilename(d)
	_, err := os.Stat(cname)
	if err == nil {
		return nil
	}
	err = os.MkdirAll(filepath.Dir(cname), 0755)
	if err != nil {
		return err
	}
	_, err, shared := c.singleflight.Do(cname, func() (any, error) {
		w, err := os.Create(cname)
		if err != nil {
			return nil, err
		}
		gw := gzip.NewWriter(w)
		_, err = gw.Write(buf)
		if err != nil {
			w.Close()
			return nil, err
		}
		err = gw.Close()
		if err != nil {
			w.Close()
			return nil, err
		}
		err = w.Close()
		return nil, err
	})
	glog.Infof("write cache content %s for %s shared:%t: %v", d, fname, shared, err)
	return err
}

// HasContent checks whether content of the digest exists in the local cache.
func (c *LocalCache) HasContent(ctx context.Context, d digest.Digest) bool {
	cname := c.contentCacheFilename(d)
	_, err := os.Stat(cname)
	return err == nil
}

func garbageCollect(ctx context.Context, dir string, threshold time.Time) (nFiles int, spaceReclaimed int64) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		glog.Warningf("Failed to read %s: %v", dir, err)
		return 0, 0
	}

	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		if entry.IsDir() {
			n, s := garbageCollect(ctx, path, threshold)
			nFiles += n
			spaceReclaimed += s
		} else {
			// There's no OS-independent way to use atime, so we just use mtime and
			// ensure that when we read a file we also update the mtime.
			info, err := os.Stat(path)
			if err != nil {
				glog.Warningf("Failed to stat file %s: %v", path, err)
				continue
			}
			if info.ModTime().Before(threshold) {
				if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
					glog.Warningf("Failed to delete %s: %v", filepath.Join(dir, entry.Name()), err)
				}
				nFiles += 1
				spaceReclaimed += info.Size()
			}
		}
	}
	return nFiles, spaceReclaimed
}

func (c *LocalCache) needsGarbageCollection(ttl time.Duration) bool {
	if c == nil {
		return false
	}
	bytes, err := os.ReadFile(filepath.Join(c.dir, "lastgc"))
	if err != nil {
		if _, err := os.Stat(c.dir); os.IsNotExist(err) {
			return false
		}
		return true
	}
	lastgc, err := strconv.ParseInt(string(bytes), 10, 64)
	if err != nil {
		return true
	}
	return c.timestamp.After(time.Unix(0, lastgc).Add(ttl))
}

func (c *LocalCache) garbageCollect(ctx context.Context, ttl time.Duration) {
	if c == nil {
		return
	}
	spin := ui.Default.NewSpinner()
	spin.Start("Performing garbage collection on the local cache")

	glog.Infof("Performing garbage collection on the local cache")
	// God this is gross. time.Sub only takes in times, and time.Add only takes
	// in durations.
	threshold := c.timestamp.Add(-ttl)
	nFiles, spaceReclaimed := garbageCollect(ctx, filepath.Join(c.dir, "contents"), threshold)
	nActions, sActions := garbageCollect(ctx, filepath.Join(c.dir, "actions"), threshold)
	nFiles += nActions
	spaceReclaimed += sActions
	if nFiles > 0 {
		glog.Infof("Garbage collected local cache: Removed %d files totalling %d MB", nFiles, spaceReclaimed/1000000)
	}

	if err := os.WriteFile(filepath.Join(c.dir, "lastgc"), []byte(fmt.Sprint(c.timestamp.UnixNano())), 0644); err != nil {
		glog.Warningf("Failed to record last garbage collection event: %v", err)
	}
	spin.Stop(nil)
}

// GarbageCollectIfRequired performs garbage collection if it has not been performed within localCacheTTL.
func (c *LocalCache) GarbageCollectIfRequired(ctx context.Context) {
	if c.needsGarbageCollection(localCacheTTL) {
		c.garbageCollect(ctx, localCacheTTL)
	}
}

// Source returns digest source for fname identified by the digest.
func (c *LocalCache) Source(_ context.Context, d digest.Digest, fname string) digest.Source {
	return dataSource{c: c, d: d, fname: fname}
}

type dataReadCloser struct {
	io.ReadCloser
	f io.Closer
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
	return err
}

type dataSource struct {
	c     *LocalCache
	d     digest.Digest
	fname string
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
			glog.Warningf("failed to open cached-digest data %s for %s: %v %v", s.d, s.fname, err, err2)
			return nil, err
		}
		glog.Infof("use %s (failed to open cached-digest data %s: %v)", s.fname, s.d, err)
		return &dataReadCloser{ReadCloser: r}, nil
	}
	gr, err := gzip.NewReader(r)
	if err != nil {
		r.Close()
		glog.Warningf("failed to gunzip cached-digest data %s for %s: %v", s.d, s.fname, err)
		return nil, err
	}
	return &dataReadCloser{ReadCloser: gr, f: r}, nil
}

func (s dataSource) String() string {
	return fmt.Sprintf("cache %s for %s", s.d, s.fname)
}
