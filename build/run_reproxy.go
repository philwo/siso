// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"time"

	"infra/build/siso/o11y/clog"
	"infra/build/siso/o11y/trace"
)

func (b *Builder) runReproxy(ctx context.Context, step *Step) error {
	ctx, span := trace.NewSpan(ctx, "run-reproxy")
	defer span.Close(nil)
	clog.Infof(ctx, "run reproxy %s", step.cmd.Desc)
	err := b.reproxySema.Do(ctx, func(ctx context.Context) error {
		started := time.Now()
		clog.Infof(ctx, "step state: remote exec (via reproxy)")
		step.setPhase(stepRemoteRun)
		err := b.reproxyExec.Run(ctx, step.cmd)
		step.setPhase(stepOutput)
		b.stats.remoteDone(ctx, err)
		if err == nil {
			step.metrics.IsRemote = true
		}
		step.metrics.RunTime = IntervalMetric(time.Since(started))
		step.metrics.done(ctx, step)
		return err
	})
	if err != nil {
		return err
	}
	// TODO(b/273407069): this won't be useful until we add code to specifically handle the deps log that reproxy returns.
	if err = b.updateDeps(ctx, step); err != nil {
		clog.Warningf(ctx, "failed to update deps: %v", err)
	}
	return err
}
