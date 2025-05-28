// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build windows

package ninja

import (
	"context"

	"go.chromium.org/infra/build/siso/build"
)

func (c *ninjaCmdRun) checkResourceLimits(ctx context.Context, limits build.Limits) {
}
