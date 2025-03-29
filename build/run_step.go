// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/charmbracelet/log"
	"go.chromium.org/infra/build/siso/reapi"
)

// StepError is step execution error.
type StepError struct {
	Target string
	Cause  error
}

func (e StepError) Error() string {
	return e.Cause.Error()
}

func (e StepError) Unwrap() error {
	return e.Cause
}

// runStep runs a step.
//
//   - check if up-to-date. do nothing if so.
//   - expand inputs with step defs (except deps=gcc)
//   - run handler if set.
//   - setup RSP file.
//   - try cmd with deps log cache (fast deps)
//   - if failed, preproc runs deps command (e.g.clang -M)
//   - run cmd
func (b *Builder) runStep(ctx context.Context, step *Step) (err error) {
	step.startTime = time.Now()

	if !b.needToRun(ctx, step.def, step.outputs) {
		b.plan.done(step)
		b.stats.update(true)
		return nil
	}

	defer func() {
		if r := recover(); r != nil {
			panic(r) // re-throw panic, handled in *Builder.Build.
		}
		if errors.Is(err, context.Canceled) {
			return
		}
		b.stats.update(false)
	}()

	stepManifest := newStepManifest(ctx, step.def)
	select {
	case <-ctx.Done():
		// interrupt during needToRun.
		return context.Cause(ctx)
	default:
	}

	step.init(ctx, b, stepManifest)

	if b.dryRun {
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		default:
		}
		log.Info(step.def.Binding("command"))
		b.plan.done(step)
		return nil
	}

	b.progressStepStarted(step)
	b.statusReporter.BuildStepStarted(step)
	defer b.progressStepFinished(step)
	defer b.statusReporter.BuildStepFinished(step)

	step.setPhase(stepHandler)
	exited, err := b.handleStep(ctx, step)
	if err != nil {
		res := cmdOutput(ctx, cmdOutputResultFAILED, step.cmd, step.def.Binding("command"), step.def.RuleName(), err)
		step.cmd.SetOutputResult(b.logOutput(res, step.cmd.Console))
		log.Warnf("Failed to exec(handle): %v", err)
		return fmt.Errorf("failed to run handler for %s: %w", step, err)
	} else if exited {
		// store handler generated outputs to local disk.
		// better to upload to CAS, or store in fs_state?
		err = b.hashFS.Flush(ctx, step.cmd.ExecRoot, step.cmd.Outputs)
		if err == nil {
			b.plan.done(step)
			return nil
		}
		log.Warnf("handle step failure: %v", err)
	}
	err = b.setupRSP(ctx, step)
	if err != nil {
		res := cmdOutput(ctx, cmdOutputResultFAILED, step.cmd, step.def.Binding("command"), step.def.RuleName(), err)
		step.cmd.SetOutputResult(b.logOutput(res, step.cmd.Console))
		return fmt.Errorf("failed to setup rsp: %s: %w", step, err)
	}
	defer func() {
		if err != nil && !errors.Is(err, context.Canceled) {
			// force flush to disk
			ferr := b.hashFS.Flush(ctx, step.cmd.ExecRoot, []string{step.cmd.RSPFile})
			log.Warnf("failed to exec %v: preserve rsp=%s flush:%v", err, step.cmd.RSPFile, ferr)
			return
		}
		b.teardownRSP(ctx, step)
	}()

	// expand inputs to get full action inputs unless deps=gcc with main supported source files such as .c, .cc, .mm etc.
	// deps gcc for rust and cxx module compiles will still rely on `depsExpandInputs` instead of scandeps.
	depsExpandInputs(ctx, b, step)

	runCmd := b.runStrategy(step)
	err = runCmd(ctx, step)
	if err != nil {
		if ctx.Err() != nil {
			err = fmt.Errorf("ctx err: %w: %w", ctx.Err(), err)
		}
		switch {
		case errors.Is(err, context.Canceled):
			// do nothing
			return err
		case errors.Is(err, reapi.ErrBadPlatformContainerImage):
			// RBE returns permission denied when
			// platform container image are not available
			// on RBE worker.
			res := cmdOutput(ctx, cmdOutputResultFAILED, step.cmd, step.def.Binding("command"), step.def.RuleName(), err)
			step.cmd.SetOutputResult(b.logOutput(res, step.cmd.Console))
		default:
			msgs := cmdOutput(ctx, cmdOutputResultFAILED, step.cmd, step.def.Binding("command"), step.def.RuleName(), err)
			step.cmd.SetOutputResult(b.logOutput(msgs, step.cmd.Console))
		}
		return StepError{
			Target: b.path.MaybeToWD(step.cmd.Outputs[0]),
			Cause:  err,
		}
	}

	res := cmdOutput(ctx, cmdOutputResultSUCCESS, step.cmd, step.def.Binding("command"), step.def.RuleName(), nil)
	if res != nil {
		step.cmd.SetOutputResult(b.logOutput(res, step.cmd.Console))
	}
	b.plan.done(step)
	return nil
}

func (b *Builder) handleStep(ctx context.Context, step *Step) (bool, error) {
	err := step.def.Handle(ctx, step.cmd)
	if err != nil {
		return false, err
	}
	result := step.cmd.ActionResult()
	exited := result != nil
	return exited, nil
}
