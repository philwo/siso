// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build !linux

package build

import "context"

func isLocalFilesystem(ctx context.Context) bool {
	return true
}
