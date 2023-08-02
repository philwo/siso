// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build linux

package cred

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// https://fuchsia.googlesource.com/fuchsia/+/ba3ebe3223ab95245f974d11f1f0c960dbabbf50/build/bazel/templates/template.bazelrc#73
// ENOKEY when missing to run `gcert`. http://shortn/_WS1VNAwslp
const googleCredHelper = "/google/src/head/depot/google3/devtools/blaze/bazel/credhelper/credhelper"

// DefaultCredentialHelper returns default credential helper's path.
func DefaultCredentialHelper() string {
	if fi, err := os.Stat(googleCredHelper); (err == nil && fi.Mode()&0111 != 0) || errors.Is(err, syscall.ENOKEY) {
		return googleCredHelper
	}
	return ""
}

func credHelperErr(fname string, err error) error {
	if fname == googleCredHelper && errors.Is(err, syscall.ENOKEY) {
		return fmt.Errorf("need to run `gcert`: %w", syscall.ENOKEY)
	}
	return err
}
