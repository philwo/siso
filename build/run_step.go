// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"

	log "github.com/golang/glog"

	"infra/build/siso/o11y/clog"
	"infra/build/siso/o11y/trace"
	"infra/build/siso/reapi"
)

// runStep runs a step.
//
//   - check if up-to-date. do nothing if so.
//   - expand inputs with step defs (except deps=gcc, msvc)
//   - run handler if set.
//   - setup RSP file.
//   - try cmd with deps log cache (fast deps)
//   - if failed, preproc runs deps command (e.g.clang -M)
//   - run cmd
//
// can control the flows with the experiment ids, defined in experiments.go.
func (b *Builder) runStep(ctx context.Context, step *Step) (err error) {
	defer func() {
		if r := recover(); r != nil {
			const size = 64 << 10
			buf := make([]byte, size)
			buf = buf[:runtime.Stack(buf, false)]
			clog.Errorf(ctx, "runStep panic %v\nstep.cmd: %p\n%s", r, step.cmd, buf)
			err = fmt.Errorf("panic: %v", r)
		}
	}()

	if log.V(1) {
		clog.Infof(ctx, "run step %s", step)
	}
	// defer some initialization after mtimeCheck?
	if step.def.IsPhony() {
		return b.phonyDone(ctx, step)
	}

	step.Init(ctx, b)
	ctx, span := trace.NewSpan(ctx, "run-step")
	defer span.Close(nil)

	skip := b.checkUpToDate(ctx, step)
	if skip {
		step.metrics.skip = true
		return b.done(ctx, step)
	}

	if b.dryRun {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		fmt.Printf("%s\n", step.cmd.Command())
		return b.done(ctx, step)
	}

	b.progressStepStarted(ctx, step)
	defer b.progressStepFinished(ctx, step)
	depsExpandInputs(ctx, b, step)
	err = b.handleStep(ctx, step)
	if err != nil {
		if !experiments.Enabled("keep-going-handle-error", "handle %s failed: %v", step, err) {
			b.failedToRun(ctx, step.cmd, err)
			return fmt.Errorf("failed to run handler for %s: %w", step, err)
		}
	} else if step.cmd.ActionResult() != nil {
		// store handler generated outputs to local disk.
		// better to upload to CAS, or store in fs_state?
		clog.Infof(ctx, "outputs[handler] %d", len(step.cmd.Outputs))
		err = b.hashFS.Flush(ctx, step.cmd.ExecRoot, step.cmd.Outputs)
		if err == nil {
			return b.done(ctx, step)
		}
		clog.Warningf(ctx, "handle step failure: %v", err)
	}

	err = b.setupRSP(ctx, step)
	if err != nil {
		b.failedToRun(ctx, step.cmd, err)
		return fmt.Errorf("failed to setup rsp: %s: %w", step, err)
	}
	defer func() {
		if err != nil && !errors.Is(err, context.Canceled) {
			clog.Warningf(ctx, "failed to exec %v: preserve rsp=%s", err, step.cmd.RSPFile)
			return
		}
		b.teardownRSP(ctx, step)
	}()

	if fastStep, ok := fastDepsCmd(ctx, b, step); ok {
		ok, err := b.tryFastStep(ctx, step, fastStep)
		if ok {
			return err
		}
	}
	step.SetPhase(stepPreproc)
	b.preprocSema.Do(ctx, func(ctx context.Context) error {
		b.stats.preprocStart(ctx)
		preprocCmd(ctx, b, step)
		b.stats.preprocEnd(ctx)
		return nil
	})
	err = b.runCmdWithCache(ctx, step, true)
	stdout := step.cmd.Stdout()
	stderr := step.cmd.Stderr()
	clog.Infof(ctx, "done err=%v", err)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			b.failedToRun(ctx, step.cmd, err)
		}
		return err
	}
	if (len(stdout) > 0 || len(stderr) > 0) && experiments.Enabled("fail-on-stdouterr", "step %s emit stdout=%d stderr=%d", step, len(stdout), len(stderr)) {
		b.failedToRun(ctx, step.cmd, err)
		return fmt.Errorf("%s emit stdout/stderr", step)
	}
	return b.done(ctx, step)
}

func (b *Builder) handleStep(ctx context.Context, step *Step) error {
	ctx, span := trace.NewSpan(ctx, "handle-step")
	defer span.Close(nil)
	return step.def.Handle(ctx, step.cmd)
}

func (b *Builder) tryFastStep(ctx context.Context, step, fastStep *Step) (bool, error) {
	// allow local run if remote exec is not set.
	// i.e. don't run local fallback due to remote exec failure
	// because it might be bad fast-deps.
	fctx, fastSpan := trace.NewSpan(ctx, "fast-deps-run")
	err := b.runCmdWithCache(fctx, fastStep, b.remoteExec == nil)
	fastSpan.Close(nil)
	if err == nil {
		b.stats.fastDepsSuccess(ctx)
		step.metrics = fastStep.metrics
		step.metrics.DepsLog = true
		stdout := fastStep.cmd.Stdout()
		stderr := fastStep.cmd.Stderr()
		clog.Infof(ctx, "fast done err=%v", err)
		if (len(stdout) > 0 || len(stderr) > 0) && experiments.Enabled("fail-on-stdouterr", "step %s emit stdout=%d stderr=%d", step, len(stdout), len(stderr)) {
			b.failedToRun(ctx, step.cmd, err)
			return true, fmt.Errorf("%s emit stdout/stderr", step)
		}
		os.Stdout.Write(stdout)
		os.Stderr.Write(stderr)
		return true, b.done(ctx, fastStep)
	}
	if errors.Is(err, context.Canceled) {
		return true, err
	}
	if errors.Is(err, reapi.ErrBadPlatformContainerImage) {
		// RBE returns permission denied when
		// platform container image are not available
		// on RBE worker.
		b.failedToRun(ctx, step.cmd, err)
		return true, err
	}
	b.stats.fastDepsFailed(ctx, err)
	if experiments.Enabled("no-fast-deps-fallback", "fast-deps %s failed", step) {
		return true, fmt.Errorf("fast-deps failed: %w", err)
	}
	return false, nil
}
