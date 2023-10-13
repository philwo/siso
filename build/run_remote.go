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
//  1. Try running a remote execution with deps log.
//  2. If it failed, it will retry a remote execution with deps scan.
//  3. If it still failed, it will fallback to local execution.
//
// - Before each remote exec, it checks remote cache before running.
// - The fallbacks can be disabled via experiment flags.
func (b *Builder) runRemote(ctx context.Context, step *Step) error {
	if fastStep, ok := fastDepsCmd(ctx, b, step); ok {
		err := b.tryFastStep(ctx, step, fastStep)
		if !errors.Is(err, errDepsLog) {
			return err
		}
	}
	step.setPhase(stepPreproc)
	err := b.preprocSema.Do(ctx, func(ctx context.Context) error {
		preprocCmd(ctx, b, step)
		return nil
	})
	if err != nil {
		return err
	}
	dedupInputs(ctx, step.cmd)
	err = b.runRemoteStep(ctx, step)
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

func (b *Builder) tryFastStep(ctx context.Context, step, fastStep *Step) error {
	// allow local run if remote exec is not set.
	// i.e. don't run local fallback due to remote exec failure
	// because it might be bad fast-deps.
	fctx, fastSpan := trace.NewSpan(ctx, "fast-deps-run")
	err := b.runRemoteStep(fctx, fastStep)
	fastSpan.Close(nil)
	if err == nil {
		step.metrics = fastStep.metrics
		step.metrics.DepsLog = true
		msgs := cmdOutput(ctx, "SUCCESS:", fastStep.cmd, step.def.Binding("command"), step.def.RuleName(), nil)
		clog.Infof(ctx, "fast done err=%v", err)
		if len(msgs) > 0 {
			b.logOutput(ctx, msgs, step.cmd.Console)
			if experiments.Enabled("fail-on-stdouterr", "step %s emit stdout/stderr", step) {
				return fmt.Errorf("%s emit stdout/stderr", step)
			}
		}
		return nil
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

func (b *Builder) runRemoteStep(ctx context.Context, step *Step) error {
	if b.cache != nil && b.reCacheEnableRead {
		err := b.execRemoteCache(ctx, step)
		if err == nil {
			return nil
		}
		clog.Infof(ctx, "cmd cache miss: %v", err)
	}
	return b.execRemote(ctx, step)
}
