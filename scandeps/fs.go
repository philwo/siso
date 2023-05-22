// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package scandeps

import (
	"context"
	"io/fs"

	"infra/build/siso/hashfs"
)

// filesystem is mirror of hashfs to optimize for scandeps access pattern.
// it is shared for all scandeps processes.
// without this, hashfs would have lots of negative caches for non-existing
// header files for every include directory.
type filesystem struct {
	hashfs *hashfs.HashFS

	// TODO(b/282888305) implement this
}

func (fsys *filesystem) ReadDir(ctx context.Context, execRoot, dname string) (map[string]bool, error) {
	// TODO(b/282888305) implement this
	return nil, fs.ErrNotExist
}
