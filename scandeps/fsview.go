// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package scandeps

import (
	"context"
	"io/fs"
	"runtime"

	"infra/build/siso/sync/semaphore"
)

var cppScanSema = semaphore.New("cppscan", runtime.NumCPU())

// fsview is a view of filesystem per scandeps process.
// It will reduce unnecessary contention to filesystem.
type fsview struct {
	fs        *filesystem
	execRoot  string
	inputDeps map[string][]string

	sysroots []string

	searchPaths []string

	// TODO(b/282888305) implement this
}

func (fv *fsview) addDir(ctx context.Context, dir string, searchPath bool) {
	// TODO(b/282888305) implement this
}

func (fv *fsview) get(ctx context.Context, dir, name string) (string, *scanResult, error) {
	// TODO(b/282888305) implement this
	return "", nil, fs.ErrNotExist
}

func (fv *fsview) results() []string {
	// TODO(b/282888305) implement this
	return nil
}
