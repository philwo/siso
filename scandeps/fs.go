// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package scandeps

import (
	"context"
	"io/fs"
	"path/filepath"
	"sync"

	"infra/build/siso/hashfs"
)

// filesystem is mirror of hashfs to optimize for scandeps access pattern.
// it is shared for all scandeps processes.
// without this, hashfs would have lots of negative caches for non-existing
// header files for every include directory.
type filesystem struct {
	hashfs *hashfs.HashFS

	files sync.Map // filename -> *scanResult
	// TODO(b/282888305) implement this
}

func (fsys *filesystem) ReadDir(ctx context.Context, execRoot, dname string) (map[string]bool, error) {
	// TODO(b/282888305) implement this
	return nil, fs.ErrNotExist
}

func (fsys *filesystem) getFile(execRoot, fname string) (*scanResult, bool) {
	v, ok := fsys.files.Load(filepath.Join(execRoot, fname))
	if !ok {
		return nil, false
	}
	sr, ok := v.(*scanResult)
	return sr, ok
}

func (fsys *filesystem) setFile(execRoot, fname string, sr *scanResult) {
	fsys.files.Store(filepath.Join(execRoot, fname), sr)
}
