// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build !unix

package osfs

import (
	"io/fs"
	"os"
)

func writeFile(name string, data []byte, perm fs.FileMode) error {
	return os.WriteFile(name, data, perm)
}
