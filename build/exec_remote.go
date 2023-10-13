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

func (b *Builder) execRemote(ctx context.Context, step *Step) error {
	ctx, span := trace.NewSpan(ctx, "exec-remote")
	defer span.Close(nil)
	clog.Infof(ctx, "exec remote %s", step.cmd.Desc)
	err := b.remoteSema.Do(ctx, func(ctx context.Context) error {
		started := time.Now()
		step.metrics.ActionStartTime = IntervalMetric(started.Sub(b.start))
		ctx = reapi.NewContext(ctx, &rpb.RequestMetadata{
			ActionId:                step.cmd.ID,
			ToolInvocationId:        b.id,
			CorrelatedInvocationsId: b.jobID,
			ActionMnemonic:          step.def.ActionName(),
			TargetId:                step.cmd.Outputs[0],
		})
		clog.Infof(ctx, "step state: remote exec")
		step.setPhase(stepRemoteRun)
		err := b.remoteExec.Run(ctx, step.cmd)
		step.setPhase(stepOutput)
		step.metrics.IsRemote = true
		_, cached := step.cmd.ActionResult()
		if cached {
			step.metrics.Cached = true
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
	return b.outputs(ctx, step)
}

func (b *Builder) execRemoteCache(ctx context.Context, step *Step) error {
	ctx, span := trace.NewSpan(ctx, "exec-remote-cache")
	defer span.Close(nil)
	err := b.cacheSema.Do(ctx, func(ctx context.Context) error {
		start := time.Now()
		step.metrics.ActionStartTime = IntervalMetric(start.Sub(b.start))
		err := b.cache.GetActionResult(ctx, step.cmd)
		if err != nil {
			return err
		}
		b.progressStepCacheHit(ctx, step)
		step.metrics.RunTime = IntervalMetric(time.Since(start))
		step.metrics.done(ctx, step)
		step.metrics.Cached = true
		return nil
	})
	if err != nil {
		return err
	}
	// need to update deps for cache hit for deps=gcc, msvc.
	// even if cache hit, deps should be updated with gcc depsfile,
	// or with msvc showIncludes outputs.
	if err = b.updateDeps(ctx, step); err != nil {
		clog.Warningf(ctx, "failed to update deps %s: %v", step, err)
	}
	return b.outputs(ctx, step)
}
