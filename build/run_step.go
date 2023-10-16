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
	"time"

	log "github.com/golang/glog"

	"infra/build/siso/execute"
	"infra/build/siso/o11y/clog"
	"infra/build/siso/o11y/trace"
	"infra/build/siso/reapi"
	"infra/build/siso/toolsupport/msvcutil"
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
			step.metrics.Err = true
		}
	}()

	if log.V(1) {
		clog.Infof(ctx, "run step %s", step)
	}
	// defer some initialization after mtimeCheck?
	if step.def.IsPhony() {
		step.metrics.skip = true
		b.plan.done(ctx, step)
		return nil
	}

	step.init(ctx, b)
	ctx, span := trace.NewSpan(ctx, "run-step")
	defer span.Close(nil)

	skip := b.checkUpToDate(ctx, step)
	if skip {
		step.metrics.skip = true
		b.plan.done(ctx, step)
		return nil
	}

	if b.dryRun {
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		default:
		}
		fmt.Printf("%s\n", step.def.Binding("command"))
		b.plan.done(ctx, step)
		return nil
	}

	b.progressStepStarted(ctx, step)
	defer b.progressStepFinished(ctx, step)
	depsExpandInputs(ctx, b, step)

	exited, err := b.handleStep(ctx, step)
	if err != nil {
		if !experiments.Enabled("keep-going-handle-error", "handle %s failed: %v", step, err) {
			msgs := cmdOutput(ctx, "FAILED[handle]:", step.cmd, step.def.Binding("command"), step.def.RuleName(), err)
			b.logOutput(ctx, msgs, step.cmd.Console)
			clog.Warningf(ctx, "Failed to exec(handle): %v", err)
			return fmt.Errorf("failed to run handler for %s: %w", step, err)
		}
	} else if exited {
		// store handler generated outputs to local disk.
		// better to upload to CAS, or store in fs_state?
		clog.Infof(ctx, "outputs[handler] %d", len(step.cmd.Outputs))
		err = b.hashFS.Flush(ctx, step.cmd.ExecRoot, step.cmd.Outputs)
		if err == nil {
			b.plan.done(ctx, step)
			return nil
		}
		clog.Warningf(ctx, "handle step failure: %v", err)
	}

	err = b.setupRSP(ctx, step)
	if err != nil {
		msgs := cmdOutput(ctx, "FAILED[rsp]:", step.cmd, step.def.Binding("command"), step.def.RuleName(), err)
		b.logOutput(ctx, msgs, step.cmd.Console)
		return fmt.Errorf("failed to setup rsp: %s: %w", step, err)
	}
	defer func() {
		if err != nil && !errors.Is(err, context.Canceled) {
			clog.Warningf(ctx, "failed to exec %v: preserve rsp=%s", err, step.cmd.RSPFile)
			return
		}
		b.teardownRSP(ctx, step)
	}()

	runCmd := b.runStrategy(ctx, step)
	err = runCmd(ctx, step)
	clog.Infof(ctx, "done err=%v", err)
	if err != nil {
		switch {
		case errors.Is(err, context.Canceled):
			// do nothing
		case errors.Is(err, reapi.ErrBadPlatformContainerImage):
			// RBE returns permission denied when
			// platform container image are not available
			// on RBE worker.
			msgs := cmdOutput(ctx, "FAILED[badContainer]:", step.cmd, step.def.Binding("command"), step.def.RuleName(), err)
			b.logOutput(ctx, msgs, step.cmd.Console)
		default:
			msgs := cmdOutput(ctx, "FAILED:", step.cmd, step.def.Binding("command"), step.def.RuleName(), err)
			b.logOutput(ctx, msgs, step.cmd.Console)
		}
		return StepError{
			Target: b.path.MustToWD(step.cmd.Outputs[0]),
			Cause:  err,
		}
	}

	msgs := cmdOutput(ctx, "SUCCESS:", step.cmd, step.def.Binding("command"), step.def.RuleName(), nil)
	if len(msgs) > 0 {
		b.logOutput(ctx, msgs, step.cmd.Console)
		if experiments.Enabled("fail-on-stdouterr", "step %s emit stdout/stderr", step) {
			return fmt.Errorf("%s emit stdout/stderr", step)
		}
	}
	b.plan.done(ctx, step)
	return nil
}

func (b *Builder) handleStep(ctx context.Context, step *Step) (bool, error) {
	ctx, span := trace.NewSpan(ctx, "handle-step")
	defer span.Close(nil)
	started := time.Now()
	err := step.def.Handle(ctx, step.cmd)
	if err != nil {
		return false, err
	}
	result, _ := step.cmd.ActionResult()
	exited := result != nil
	if exited {
		step.metrics.ActionStartTime = IntervalMetric(started.Sub(b.start))
		step.metrics.RunTime = IntervalMetric(time.Since(started))
		step.metrics.NoExec = true
	}
	return exited, nil
}

// cmdOutput returns cmd ouptut log (result, id, desc, err, action, output, args, stdout, stderr).
// it will return nil if ctx is canceled or success with no stdout/stderr.
func cmdOutput(ctx context.Context, result string, cmd *execute.Cmd, cmdline, rule string, err error) []string {
	if ctx.Err() != nil {
		return nil
	}
	stdout := cmd.Stdout()
	stderr := cmd.Stderr()
	if cmd.Deps == "msvc" {
		// cl.exe, clang-cl shows included file to stderr
		// but RBE merges stderr into stdout...
		_, stdout = msvcutil.ParseShowIncludes(stdout)
		_, stderr = msvcutil.ParseShowIncludes(stderr)
	}
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
	if err != nil {
		msgs = append(msgs, fmt.Sprintf("err: %v\n", err))
	}
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

func (b *Builder) logOutput(ctx context.Context, msgs []string, console bool) {
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
			case strings.HasPrefix(msg, "err:"):
			case strings.HasPrefix(msg, "stdout:"), strings.HasPrefix(msg, "stderr:"):
				if !console {
					fmt.Fprint(&sb, msg)
				}
			}
		}
		out := sb.String()
		if !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		ui.Default.PrintLines(out + "\n")
		return
	}
	if strings.HasPrefix(msgs[0], "FALLBACK") {
		return
	}
	if console {
		var nmsgs []string
		for _, msg := range msgs {
			switch {
			case strings.HasPrefix(msg, "stdout:"), strings.HasPrefix(msg, "stderr:"):
				continue
			}
			nmsgs = append(nmsgs, msg)
		}
		msgs = nmsgs
	}
	ui.Default.PrintLines(append([]string{"\n", "\n"}, msgs...)...)
}
