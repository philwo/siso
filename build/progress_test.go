// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"testing"
	"time"

	"infra/build/siso/execute"
	"infra/build/siso/ui"
)

func TestProgress_NotIsTerminal(t *testing.T) {
	currentUI := ui.Default
	defer func() { ui.Default = currentUI }()
	ui.Default = &ui.LogUI{}
	var p progress
	b := &Builder{
		plan:  &plan{},
		stats: &stats{},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.start(ctx, b)

	step := &Step{
		cmd: &execute.Cmd{
			Desc: "ACTION sample",
		},
		state: &stepState{},
	}
	step.setPhase(stepStart)
	p.step(ctx, b, step, progressPrefixStart)
	started := time.Now()
	var count int64
	for count == 0 && time.Since(started) < 1*time.Second {
		time.Sleep(200 * time.Millisecond)
		count = p.count.Load()
		t.Logf("count=%d", count)
	}
	if count == 0 {
		t.Errorf("progress count=%d; want >0", count)
	}
	step.setPhase(stepDone)
	p.step(ctx, b, step, progressPrefixFinish)
	p.stop(ctx)

	if w := step.getWeightedDuration(); w == 0 {
		t.Errorf("weighted_duration=0; want non-zero")
	}
}

func TestProgressFormatDuration(t *testing.T) {
	for _, tc := range []struct {
		dur  time.Duration
		want string
	}{
		{
			want: "0.00s",
		},
		{
			dur:  1 * time.Millisecond,
			want: "0.00s",
		},
		{
			dur:  10 * time.Millisecond,
			want: "0.01s",
		},
		{
			dur:  1 * time.Second,
			want: "1.00s",
		},
		{
			dur:  1 * time.Minute,
			want: "1m00.00s",
		},
		{
			dur:  1*time.Minute + 1*time.Second + 100*time.Millisecond,
			want: "1m01.10s",
		},
	} {
		got := formatDuration(tc.dur)
		if got != tc.want {
			t.Errorf("formatDuration(%v)=%q; want=%q", tc.dur, got, tc.want)
		}
	}
}
