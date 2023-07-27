// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build linux

package main

import (
	"errors"
	"os"
	"syscall"
)

func defaultCredentialHelper() string {
	const googleCredHelper = "/google/src/head/depot/google3/devtools/blaze/bazel/credhelper/credhelper"
	if fi, err := os.Stat(googleCredHelper); (err == nil && fi.Mode()&0111 != 0) || errors.Is(err, syscall.ENOKEY) {
		return googleCredHelper
	}
	return ""
}
