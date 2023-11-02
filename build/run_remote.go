// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"errors"
	"fmt"

	"infra/build/siso/o11y/clog"
	"infra/build/siso/o11y/trace"
	"infra/build/siso/reapi"
)

var errDepsLog = errors.New("failed to exec with deps log")

// runRemote runs step with using remote apis.
//
//  1. Check remote cacche with deps log if available.
//  2. If local resource is idle, run locally.
//  3. Otherwise, try running a remote execution with deps log.
//  4. If it failed, it will retry a remote execution with deps scan.
//  5. If it still failed, it will fallback to local execution.
//
// - Before each remote exec, it checks remote cache before running.
// - The fallbacks can be disabled via experiment flags.
func (b *Builder) runRemote(ctx context.Context, step *Step) error {
	var fastStep *Step
	var fastOK, fastChecked bool
	fastNeedCheckCache := true
	cacheCheck := b.cache != nil && b.reCacheEnableRead
	if b.fastLocalSema != nil && int(b.progress.numLocal.Load()) < b.fastLocalSema.Capacity() {
		// TODO: skip fast when step is too new and can't expect cache hit?
		if cacheCheck {
			fastStep, fastOK = fastDepsCmd(ctx, b, step)
			if fastOK {
				err := b.execRemoteCache(ctx, fastStep)
				if err == nil {
					return b.fastStepDone(ctx, step, fastStep)
				}
				fastNeedCheckCache = false
				clog.Infof(ctx, "cmd fast cache miss: %v", err)
			}
			fastChecked = true
		}
		if ctx, done, err := b.fastLocalSema.TryAcquire(ctx); err == nil {
			var err error
			defer func() { done(err) }()
			clog.Infof(ctx, "fast local %s", step.cmd.Desc)
			// TODO: check cache if input age is old enough.
			// TODO: detach remote for future cache hit.
			err = b.execLocal(ctx, step)
			step.metrics.FastLocal = true
			return err
		}
	}
	if !fastChecked {
		fastStep, fastOK = fastDepsCmd(ctx, b, step)
	}
	if fastOK {
		err := b.tryFastStep(ctx, step, fastStep, fastNeedCheckCache && cacheCheck)
		if !errors.Is(err, errDepsLog) {
			return err
		}
	}
	step.setPhase(stepPreproc)
	err := b.preprocSema.Do(ctx, func(ctx context.Context) error {
		ctx, span := trace.NewSpan(ctx, "preproc")
		defer span.Close(nil)
		err := depsCmd(ctx, b, step)
		if err != nil {
			// disable remote execution. b/289143861
			step.cmd.Platform = nil
			return fmt.Errorf("disable remote: failed to get %s deps: %w", step.cmd.Deps, err)
		}
		return nil
	})
	if err == nil {
		dedupInputs(ctx, step.cmd)
		err = b.runRemoteStep(ctx, step, cacheCheck)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return err
		}
		if errors.Is(err, reapi.ErrBadPlatformContainerImage) {
			return err
		}
		if errors.Is(err, errNotRelocatable) {
			clog.Errorf(ctx, "not relocatable: %v", err)
			return err
		}
		if experiments.Enabled("no-fallback", "remote-exec %s failed. no-fallback", step) {
			return fmt.Errorf("remote-exec %s failed no-fallback: %w", step.cmd.ActionDigest(), err)
		}
		step.metrics.IsRemote = false
		step.metrics.Fallback = true
		msgs := cmdOutput(ctx, "FALLBACK", step.cmd, step.def.Binding("command"), step.def.RuleName(), err)
		b.logOutput(ctx, msgs, false)
		err = b.execLocal(ctx, step)
		if err != nil {
			return err
		}
	}
	return err
}

func (b *Builder) tryFastStep(ctx context.Context, step, fastStep *Step, cacheCheck bool) error {
	fctx, fastSpan := trace.NewSpan(ctx, "fast-deps-run")
	err := b.runRemoteStep(fctx, fastStep, cacheCheck)
	fastSpan.Close(nil)
	if err == nil {
		return b.fastStepDone(ctx, step, fastStep)
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	if errors.Is(err, reapi.ErrBadPlatformContainerImage) {
		return err
	}
	step.metrics.DepsLogErr = true
	if experiments.Enabled("no-fast-deps-fallback", "fast-deps %s failed", step) {
		return fmt.Errorf("fast-deps failed: %w", err)
	}
	return errDepsLog
}

func (b *Builder) fastStepDone(ctx context.Context, step, fastStep *Step) error {
	step.metrics = fastStep.metrics
	step.metrics.DepsLog = true
	msgs := cmdOutput(ctx, "SUCCESS:", fastStep.cmd, step.def.Binding("command"), step.def.RuleName(), nil)
	if len(msgs) > 0 {
		b.logOutput(ctx, msgs, step.cmd.Console)
		if experiments.Enabled("fail-on-stdouterr", "step %s emit stdout/stderr", step) {
			return fmt.Errorf("%s emit stdout/stderr", step)
		}
	}
	return nil
}

func (b *Builder) runRemoteStep(ctx context.Context, step *Step, cacheCheck bool) error {
	if len(step.cmd.Platform) == 0 || step.cmd.Platform["container-image"] == "" {
		return fmt.Errorf("no remote available (missing container-image property)")
	}
	if cacheCheck {
		err := b.execRemoteCache(ctx, step)
		if err == nil {
			return nil
		}
		clog.Infof(ctx, "cmd cache miss: %v", err)
	}
	return b.execRemote(ctx, step)
}
