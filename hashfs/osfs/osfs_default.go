// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build !darwin

package osfs

import "errors"

const HasClonefile = false

func Clonefile(src, dst string) error {
	return errors.ErrUnsupported
}
