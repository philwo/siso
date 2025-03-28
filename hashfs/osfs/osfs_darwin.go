// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build darwin

package osfs

import (
	"golang.org/x/sys/unix"
)

// Clonefile copies src to dst by using clonefile.
func (fs *OSFS) Clonefile(src, dst string) error {
	return unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW)
}
