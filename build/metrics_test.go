// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"testing"
	"time"

	"infra/build/siso/execute"
)

func TestStepMetricsDone_NoExecutionMetadata(t *testing.T) {
	ctx := context.Background()
	step := &Step{
		state: &stepState{},
		cmd:   &execute.Cmd{},
	}
	var m StepMetric
	m.done(ctx, step, time.Now())
	t.Logf("m.done passed without panic")
}
