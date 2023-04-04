// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"time"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"

	"infra/build/siso/o11y/clog"
	"infra/build/siso/o11y/trace"
	"infra/build/siso/reapi"
)

func (b *Builder) runRemote(ctx context.Context, step *Step) error {
	ctx, span := trace.NewSpan(ctx, "run-remote")
	defer span.Close(nil)
	clog.Infof(ctx, "run remote %s", step.cmd.Desc)
	err := b.remoteSema.Do(ctx, func(ctx context.Context) error {
		started := time.Now()
		ctx = reapi.NewContext(ctx, &rpb.RequestMetadata{
			ActionId:         step.cmd.ID,
			ToolInvocationId: b.id,
			ActionMnemonic:   step.Def.ActionName(),
			TargetId:         step.cmd.Outputs[0],
		})
		clog.Infof(ctx, "step state: remote exec")
		step.SetPhase(stepRemoteRun)
		err := b.remoteExec.Run(ctx, step.cmd)
		step.SetPhase(stepOutput)
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
	// need to update deps for remote exec for deps=gcc with depsfile,
	// or deps=msvc with showIncludes
	if err = b.updateDeps(ctx, step); err != nil {
		clog.Warningf(ctx, "failed to update deps: %v", err)
	}
	return err
}
