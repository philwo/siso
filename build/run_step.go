// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"

	log "github.com/golang/glog"

	"infra/build/siso/execute"
	"infra/build/siso/o11y/clog"
	"infra/build/siso/o11y/trace"
	"infra/build/siso/reapi"
	"infra/build/siso/ui"
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
			clog.Errorf(ctx, "runStep panic: %v\nstep.cmd: %p\n%s", r, step.cmd, buf)
			err = fmt.Errorf("panic: %v: %s", r, buf)
		}
		if err != nil {
			b.stats.fail()
		}
	}()

	if log.V(1) {
		clog.Infof(ctx, "run step %s", step)
	}
	// defer some initialization after mtimeCheck?
	if step.def.IsPhony() {
		b.stats.skipped(ctx)
		step.metrics.skip = true
		return b.phonyDone(ctx, step)
	}

	step.init(ctx, b)
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
		fmt.Printf("%s\n", step.def.Binding("command"))
		return b.done(ctx, step)
	}

	b.progressStepStarted(ctx, step)
	defer b.progressStepFinished(ctx, step)
	depsExpandInputs(ctx, b, step)
	err = b.handleStep(ctx, step)
	if err != nil {
		if !experiments.Enabled("keep-going-handle-error", "handle %s failed: %v", step, err) {
			msgs := cmdOutput(ctx, "FAILED[handle]:", step.cmd, step.def.Binding("command"), step.def.RuleName(), err)
			b.logOutput(ctx, msgs)
			clog.Warningf(ctx, "Failed to exec(handle): %v", err)
			return fmt.Errorf("failed to run handler for %s: %w", step, err)
		}
	} else {
		result, _ := step.cmd.ActionResult()
		if result != nil {
			// store handler generated outputs to local disk.
			// better to upload to CAS, or store in fs_state?
			clog.Infof(ctx, "outputs[handler] %d", len(step.cmd.Outputs))
			err = b.hashFS.Flush(ctx, step.cmd.ExecRoot, step.cmd.Outputs)
			if err == nil {
				return b.done(ctx, step)
			}
			clog.Warningf(ctx, "handle step failure: %v", err)
		}
	}

	err = b.setupRSP(ctx, step)
	if err != nil {
		msgs := cmdOutput(ctx, "FAILED[rsp]:", step.cmd, step.def.Binding("command"), step.def.RuleName(), err)
		b.logOutput(ctx, msgs)
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
	step.setPhase(stepPreproc)
	b.preprocSema.Do(ctx, func(ctx context.Context) error {
		b.stats.preprocStart(ctx)
		preprocCmd(ctx, b, step)
		b.stats.preprocEnd(ctx)
		return nil
	})
	err = b.runCmdWithCache(ctx, step, true)
	clog.Infof(ctx, "done err=%v", err)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			msgs := cmdOutput(ctx, "FAILED:", step.cmd, step.def.Binding("command"), step.def.RuleName(), err)
			b.logOutput(ctx, msgs)
		}
		return StepError{
			Target: b.path.MustToWD(step.cmd.Outputs[0]),
			Cause:  err,
		}
	}

	msgs := cmdOutput(ctx, "SUCCESS:", step.cmd, step.def.Binding("command"), step.def.RuleName(), nil)
	if len(msgs) > 0 {
		b.logOutput(ctx, msgs)
		if experiments.Enabled("fail-on-stdouterr", "step %s emit stdout/stderr", step) {
			return fmt.Errorf("%s emit stdout/stderr", step)
		}
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
		msgs := cmdOutput(ctx, "SUCCESS:", fastStep.cmd, step.def.Binding("command"), step.def.RuleName(), nil)
		clog.Infof(ctx, "fast done err=%v", err)
		if len(msgs) > 0 {
			b.logOutput(ctx, msgs)
			if experiments.Enabled("fail-on-stdouterr", "step %s emit stdout/stderr", step) {
				return true, fmt.Errorf("%s emit stdout/stderr", step)
			}
		}
		return true, b.done(ctx, fastStep)
	}
	if errors.Is(err, context.Canceled) {
		return true, err
	}
	if errors.Is(err, reapi.ErrBadPlatformContainerImage) {
		// RBE returns permission denied when
		// platform container image are not available
		// on RBE worker.
		msgs := cmdOutput(ctx, "FAILED[badContainer]:", fastStep.cmd, step.def.Binding("command"), fastStep.def.RuleName(), err)
		b.logOutput(ctx, msgs)
		return true, err
	}
	b.stats.fastDepsFailed(ctx, err)
	if experiments.Enabled("no-fast-deps-fallback", "fast-deps %s failed", step) {
		return true, fmt.Errorf("fast-deps failed: %w", err)
	}
	return false, nil
}

// cmdOutput returns cmd ouptut log (result, id, desc, err, action, output, args, stdout, stderr).
// it will return nil if ctx is canceled or success with no stdout/stderr.
func cmdOutput(ctx context.Context, result string, cmd *execute.Cmd, cmdline, rule string, err error) []string {
	if ctx.Err() != nil {
		return nil
	}
	stdout := cmd.Stdout()
	stderr := cmd.Stderr()
	if err == nil && len(stdout) == 0 && len(stderr) == 0 {
		return nil
	}
	var output string
	if len(cmd.Outputs) > 0 {
		output = cmd.Outputs[0]
		if strings.HasPrefix(output, cmd.Dir+"/") {
			output = "./" + strings.TrimPrefix(output, cmd.Dir+"/")
		}
	}
	var msgs []string
	msgs = append(msgs, fmt.Sprintf("%s %s %s\n", result, cmd, cmd.Desc))
	msgs = append(msgs, fmt.Sprintf("err: %v\n", err))
	if rule != "" {
		msgs = append(msgs, fmt.Sprintf("siso_rule:%s\n", rule))
	}
	msgs = append(msgs, fmt.Sprintf("%q %q\n%s\n", cmd.ActionName, output, cmdline))
	rsp := cmd.RSPFile
	if rsp != "" {
		msgs = append(msgs, fmt.Sprintf(" %s=%q\n", rsp, cmd.RSPFileContent))
	}
	if len(stdout) > 0 {
		msgs = append(msgs, fmt.Sprintf("stdout:\n%s", string(stdout)))
	}
	if len(stderr) > 0 {
		msgs = append(msgs, fmt.Sprintf("stderr:\n%s", string(stderr)))
	}
	return msgs

}

func (b *Builder) logOutput(ctx context.Context, msgs []string) {
	if len(msgs) == 0 {
		return
	}
	if b.outputLogWriter != nil {
		fmt.Fprint(b.outputLogWriter, strings.Join(msgs, "")+"\f\n")
		if strings.HasPrefix(msgs[0], "FALLBACK") {
			return
		}
		var sb strings.Builder
		switch {
		case strings.HasPrefix(msgs[0], "FAILED"):
			fmt.Fprint(&sb, ui.SGR(ui.Red, msgs[0]))
		case strings.HasPrefix(msgs[0], "SUCCESS"):
			fmt.Fprint(&sb, ui.SGR(ui.Green, msgs[0]))
		default:
			fmt.Fprint(&sb, msgs[0])
		}
		for _, msg := range msgs {
			switch {
			case strings.HasPrefix(msg, "err:") || strings.HasPrefix(msg, "stdout:") || strings.HasPrefix(msg, "stderr:"):
				fmt.Fprint(&sb, msg)
			}
		}
		ui.Default.PrintLines(sb.String())
		return
	}
	if strings.HasPrefix(msgs[0], "FALLBACK") {
		return
	}
	ui.Default.PrintLines(append([]string{"\n", "\n"}, msgs...)...)
}
