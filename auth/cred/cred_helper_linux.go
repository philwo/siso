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
	"time"
)

// https://fuchsia.googlesource.com/fuchsia/+/ba3ebe3223ab95245f974d11f1f0c960dbabbf50/build/bazel/templates/template.bazelrc#73
// ENOKEY when missing to run `gcert`. http://shortn/_WS1VNAwslp
const googleCredHelper = "/google/src/head/depot/google3/devtools/blaze/bazel/credhelper/credhelper"

// DefaultCredentialHelper returns default credential helper's path.
func DefaultCredentialHelper() string {
	ch := make(chan string, 1)
	go func() {
		if fi, err := os.Stat(googleCredHelper); (err == nil && fi.Mode()&0111 != 0) || errors.Is(err, syscall.ENOKEY) {
			ch <- googleCredHelper
			return
		}
		ch <- ""
	}()
	select {
	case helper := <-ch:
		return helper
	case <-time.After(5 * time.Second):
		// workaround for b/360055934
		fmt.Fprintf(os.Stderr, `WARNING: failed to access /google/src.
probably need RPC access: http://go/request-rpc
`)
		return ""

	}
}

func credHelperErr(fname string, err error) error {
	if fname == googleCredHelper && errors.Is(err, syscall.ENOKEY) {
		return fmt.Errorf("need to run `gcert`: %w", syscall.ENOKEY)
	}
	return err
}
