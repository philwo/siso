// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build windows

package ninja

import (
	"context"
	"os"

	"github.com/charmbracelet/log"
)

func lookupUser(ctx context.Context) string {
	// user.Current is too slow on Windows on some condition
	// https://go.dev/issue/68312
	// b/351131869
	u := os.Getenv("USER")
	if u != "" {
		return u
	}
	u = os.Getenv("USERNAME")
	if u != "" {
		return u
	}
	log.Warnf("failed to get username $env:USER or $env:USERNAME")
	return "unknownuser"
}
