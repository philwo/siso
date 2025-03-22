// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build darwin

package osfs

import (
	"time"

	"golang.org/x/sys/unix"
)

// Clonefile copies src to dst by using cloneline.
func (fs *OSFS) Clonefile(src, dst string) error {
	started := time.Now()
	err := unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW)
	if dur := time.Since(started); dur > 1*time.Minute {
		logSlow(dst, dur, err)
	}
	return err
}
