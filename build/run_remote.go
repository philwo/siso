// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"errors"
	"fmt"

	"github.com/charmbracelet/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"go.chromium.org/infra/build/siso/execute"
	"go.chromium.org/infra/build/siso/reapi"
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
				log.Infof("cmd fast cache miss: %v", err)
			}
			fastChecked = true
		}
		if ctx, done, err := b.fastLocalSema.TryAcquire(ctx); err == nil {
			var err error
			defer func() { done(err) }()
			log.Infof("fast local %s", step.cmd.Desc)
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
		err := depsCmd(ctx, b, step)
		if err != nil {
			// disable remote execution. b/289143861
			step.cmd.Platform = nil
			return fmt.Errorf("disable remote: failed to get %s deps: %w", step.cmd.Deps, err)
		}
		return nil
	})
	if err == nil {
		dedupInputs(step.cmd)
		err = b.runRemoteStep(ctx, step, cacheCheck)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return err
		}
		if errors.Is(err, reapi.ErrBadPlatformContainerImage) {
			return err
		}
		if status.Code(err) == codes.PermissionDenied {
			return err
		}
		if errors.Is(err, errNotRelocatable) {
			log.Errorf("not relocatable: %v", err)
			return err
		}
		var eerr execute.ExitError
		if errors.As(err, &eerr) && len(step.cmd.Stdout())+len(step.cmd.Stderr()) > 0 && b.failures.allowed == 1 {
			var output string
			if len(step.cmd.Outputs) > 0 {
				output = step.cmd.Outputs[0]
			}
			switch {
			case eerr.ExitCode == 137:
				log.Warnf("Fallback due to potential SIGKILL by docker: remote exec %s failed: output=%q siso_config=%q, gn_target=%q: %v", step.cmd.ActionDigest(), output, step.def.RuleName(), step.def.Binding("gn_target"), err)
			case experiments.Enabled("fallback-on-exec-error", "remote exec %s failed: %v", step.cmd.ActionDigest(), err):
				log.Warnf("fallback-on-exec-error: remote exec %s failed: output=%q siso_config=%q, gn_target=%q: %v", step.cmd.ActionDigest(), output, step.def.RuleName(), step.def.Binding("gn_target"), err)
			default:
				// report compile fail early to developers.
				// If user runs on non-terminal or user sets a
				// non-default -k, then it implies that they want to
				// keep going as much as possible and
				// correct result, rather than fast feedback.
				return fmt.Errorf("remote-exec %s failed: %w", step.cmd.ActionDigest(), err)
			}
		}
		if !b.localFallbackEnabled() {
			return fmt.Errorf("remote-exec %s failed no-fallback: %w", step.cmd.ActionDigest(), err)
		}
		log.Warnf("remote-exec %s failed, fallback to local: %v", step.cmd.ActionDigest(), err)
		b.progressStepFallback(step)
		step.metrics.IsRemote = false
		step.metrics.Fallback = true
		res := cmdOutput(ctx, cmdOutputResultFALLBACK, step.cmd, step.def.Binding("command"), step.def.RuleName(), err)
		b.logOutput(res, false)
		// Preserve remote action result and error.
		ar, _ := step.cmd.ActionResult()
		step.cmd.SetRemoteFallbackResult(ar, err)
		err = b.execLocal(ctx, step)
		if err != nil {
			return err
		}
	}
	return err
}

func (b *Builder) tryFastStep(ctx context.Context, step, fastStep *Step, cacheCheck bool) error {
	err := b.runRemoteStep(ctx, fastStep, cacheCheck)
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
	stats := b.stats.stats()
	nFastDeps := stats.FastDepsSuccess + stats.FastDepsFailed + 1
	if nFastDeps > 100 && (stats.FastDepsFailed+1)*100 > nFastDeps {
		// many fast-deps failure.
		// better to use scandeps to reduce retry by fast-deps failure.
		log.Infof("too many fast-deps failure detected %d/%d", stats.FastDepsFailed+1, stats.FastDepsSuccess)
		b.disableFastDeps.CompareAndSwap(nil, "too many fast-deps failure")
	}

	if experiments.Enabled("no-fast-deps-fallback", "fast-deps %s failed", step) {
		return fmt.Errorf("fast-deps failed: %w", err)
	}
	return errDepsLog
}

func (b *Builder) fastStepDone(ctx context.Context, step, fastStep *Step) error {
	step.metrics = fastStep.metrics
	step.metrics.DepsLog = true
	res := cmdOutput(ctx, cmdOutputResultSUCCESS, fastStep.cmd, step.def.Binding("command"), step.def.RuleName(), nil)
	if res != nil {
		b.logOutput(res, step.cmd.Console)
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
	}
	return b.execRemote(ctx, step)
}
