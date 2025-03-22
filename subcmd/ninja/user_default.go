// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build !windows

package ninja

import (
	"context"
	"os/user"

	"github.com/charmbracelet/log"
)

func lookupUser(ctx context.Context) string {
	current, err := user.Current()
	if err != nil {
		log.Warnf("failed to get current user: %v", err)
		return "unknownuser"
	}
	return current.Username
}
