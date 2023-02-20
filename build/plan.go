// Copyright 2023 The Chromium Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"time"
)

// StepDefs provides step definitions.
type StepDefs interface {
	// TODO(b/266518906): Migrate New() and Targets().

	// RecordDepsLog records deps log of the output.
	RecordDepsLog(ctx context.Context, output string, mtime time.Time, deps []string) (updated bool, err error)
}
