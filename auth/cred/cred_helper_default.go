// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build !linux

package cred

// DefaultCredentialHelper returns default credential helper's path.
func DefaultCredentialHelper() string {
	return ""
}

func credHelperErr(fname string, err error) error {
	return err
}
