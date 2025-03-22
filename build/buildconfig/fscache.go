// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package buildconfig

import (
	"context"
	"io/fs"
	"sync"

	"github.com/charmbracelet/log"
	"golang.org/x/sync/singleflight"
)

// fscache is a cache of contents from fs (hashfs).
// TODO(b/273878593): limit cache size in buildconfig/fscache
type fscache struct {
	mu sync.Mutex
	s  singleflight.Group
	m  map[string][]byte
}

// Get reads the file fname from fsys into memory and returns its content.
func (c *fscache) Get(ctx context.Context, fsys fs.FS, fname string) ([]byte, error) {
	c.mu.Lock()
	buf, ok := c.m[fname]
	c.mu.Unlock()
	if ok {
		log.Debugf("fscache hit %s: %d", fname, len(buf))
		return buf, nil
	}
	v, err, _ := c.s.Do(fname, func() (any, error) {
		buf, err := fs.ReadFile(fsys, fname)
		if err != nil {
			return buf, err
		}
		log.Infof("fscache set %s: %d", fname, len(buf))
		c.mu.Lock()
		c.m[fname] = buf
		c.mu.Unlock()
		return buf, err
	})
	buf = v.([]byte)
	return buf, err
}
