// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build linux

package build

import (
	"context"

	"golang.org/x/sys/unix"

	"infra/build/siso/o11y/clog"
)

func isLocalFilesystem(ctx context.Context) bool {
	var st unix.Statfs_t
	err := unix.Statfs(".", &st)
	if err != nil {
		clog.Warningf(ctx, "statfs failed: %v", err)
		return true
	}
	switch st.Type {
	case unix.FUSE_SUPER_MAGIC, unix.OVERLAYFS_SUPER_MAGIC:
		return false
	default:
		return true
	}
}
