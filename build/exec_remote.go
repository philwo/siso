// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"time"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"infra/build/siso/o11y/clog"
	"infra/build/siso/o11y/trace"
	"infra/build/siso/reapi"
	"infra/build/siso/reapi/retry"
)

func (b *Builder) execRemote(ctx context.Context, step *Step) error {
	ctx, span := trace.NewSpan(ctx, "exec-remote")
	defer span.Close(nil)
	started := time.Now()
	noFallback := experiments.Enabled("no-fallback", "no-fallback tries for reapi deadline exceeded at most 4 times")
	var timeout time.Duration
	if noFallback {
		// In no-fallback mode, remote execution will be tried 4 times at most.
		// Since an execution sets cmd.Timeout * 2 as action timeout, the 2nd try might be deduplicated by RBE scheduler. 3rd or 4th try should successfully restart a new execution.
		// Alternatively, Siso could avoid extending action timeout in no-fallback mode.
		// However, this will lose the opportunity to cache long actions.
		timeout = step.cmd.Timeout * 4
	}
	clog.Infof(ctx, "exec remote %s", step.cmd.Desc)
	err := retry.Do(ctx, func() error {
		err := b.remoteSema.Do(ctx, func(ctx context.Context) error {
			reExecStarted := time.Now()
			step.metrics.ActionStartTime = IntervalMetric(reExecStarted.Sub(b.start))
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
			step.metrics.RunTime = IntervalMetric(time.Since(reExecStarted))
			step.metrics.done(ctx, step)
			return err
		})
		dur := time.Since(started)
		if code := status.Code(err); noFallback && code == codes.DeadlineExceeded && dur < timeout {
			clog.Warningf(ctx, "exec remote timedout duration=%s timeout=%s: %v", dur, timeout, err)
			err = status.Errorf(codes.Unavailable, "reapi timedout %v", err)
		}
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
