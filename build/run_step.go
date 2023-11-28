// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"

	"cloud.google.com/go/logging"
	"google.golang.org/grpc/status"

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
	stepStart := time.Now()
	tc := trace.New(ctx, step.def.String())
	ctx = trace.NewContext(ctx, tc)
	spanName := stepSpanName(step.def)
	ctx, span := trace.NewSpan(ctx, "step:"+spanName)
	traceID, spanID := span.ID(b.projectID)
	ctx = clog.NewSpan(ctx, traceID, spanID, map[string]string{
		logLabelKeyID: step.def.String(),
	})
	logger := clog.FromContext(ctx)
	logger.Formatter = logFormat
	defer func(span *trace.Span) {
		if errors.Is(err, context.Canceled) {
			span.Close(status.FromContextError(err).Proto())
			return
		}
		if err != nil {
			step.metrics.Err = true
			st, ok := status.FromError(err)
			if !ok {
				st = status.FromContextError(err)
			}
			span.Close(st.Proto())
		} else {
			span.Close(nil)
		}
		if !step.metrics.skip {
			duration := time.Since(stepStart)
			step.metrics.Duration = IntervalMetric(duration)
			step.metrics.ActionEndTime = IntervalMetric(step.startTime.Add(duration).Sub(b.start))
			step.metrics.Err = err != nil
			stepLogEntry(ctx, logger, step, duration, err)
			b.recordMetrics(ctx, step.metrics)
			b.recordNinjaLogs(ctx, step)
			b.stats.update(ctx, &step.metrics, step.cmd.Pure)
			b.finalizeTrace(ctx, tc)
			b.outputFailureSummary(ctx, step, err)
			b.outputFailedCommands(ctx, step, err)
		}
		// unref for GC to reclaim memory.
		step.cmd = nil
	}(span)

	if step.def.IsPhony() || b.checkUpToDate(ctx, step.def, step.outputs) {
		step.metrics.skip = true
		b.plan.done(ctx, step)
		b.stats.update(ctx, &step.metrics, true)
		return nil
	}

	step.init(ctx, b)
	description := stepDescription(step.def)
	prevStepOut := b.prevStepOut(ctx, step)
	stepStartLog(logger, step, description, spanName)
	step.metrics.init(ctx, b, step, stepStart, prevStepOut)
	b.stepSpanInit(span, step, description, spanName, prevStepOut)

	ctx, span = trace.NewSpan(ctx, "run-step")
	defer span.Close(nil)

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

	step.setPhase(stepHandler)
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
			Target: b.path.MaybeToWD(step.cmd.Outputs[0]),
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

func (b *Builder) prevStepOut(ctx context.Context, step *Step) string {
	if step.prevStepOut == nil {
		return ""
	}
	s, err := b.graph.TargetPath(step.prevStepOut)
	if err != nil {
		clog.Warningf(ctx, "failed to get target path: %v", err)
		return ""
	}
	return s
}

func stepStartLog(logger *clog.Logger, step *Step, description, spanName string) {
	logEntry := logger.Entry(logging.Info, description)
	logEntry.Labels = map[string]string{
		"id":          step.def.String(),
		"command":     step.def.Binding("command"),
		"description": description,
		"action":      step.def.ActionName(),
		"span_name":   spanName,
		"output0":     step.def.Outputs()[0],
	}
	logger.Log(logEntry)
}

func (b *Builder) stepSpanInit(span *trace.Span, step *Step, description, spanName, prevStepOut string) {
	span.SetAttr("ready_time", time.Since(step.readyTime).Milliseconds())
	span.SetAttr("prev", step.prevStepID)
	span.SetAttr("prev_out", prevStepOut)
	span.SetAttr("queue_time", time.Since(step.queueTime).Milliseconds())
	span.SetAttr("queue_size", step.queueSize)
	span.SetAttr("build_id", b.id)
	span.SetAttr("id", step.def.String())
	span.SetAttr("command", step.def.Binding("command"))
	span.SetAttr("description", description)
	span.SetAttr("action", step.def.ActionName())
	span.SetAttr("span_name", spanName)
	span.SetAttr("output0", step.def.Outputs()[0])
	if next := step.def.Next(); next != nil {
		span.SetAttr("next_id", step.def.Next().String())
	}
	if step.metrics.GNTarget != "" {
		span.SetAttr("gn_target", step.metrics.GNTarget)
	}
	span.SetAttr("backtraces", stepBacktraces(step))
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
	digest := cmd.ActionDigest()
	if !digest.IsZero() {
		msgs = append(msgs, fmt.Sprintf("digest:%s\n", cmd.ActionDigest().String()))
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

func (b *Builder) outputFailureSummary(ctx context.Context, step *Step, err error) {
	if err == nil || b.failureSummaryWriter == nil || step.cmd == nil {
		return
	}
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "%s\n", step.cmd.Desc)
	fmt.Fprintf(&buf, "%s\n", strings.Join(step.cmd.Args, " "))
	stderr := step.cmd.Stderr()
	if len(stderr) > 0 {
		fmt.Fprint(&buf, ui.StripANSIEscapeCodes(string(stderr)))
	}
	stdout := step.cmd.Stdout()
	if len(stdout) > 0 {
		fmt.Fprint(&buf, ui.StripANSIEscapeCodes(string(stdout)))
	}
	fmt.Fprintf(&buf, "%v\n", err)
	_, err = b.failureSummaryWriter.Write(buf.Bytes())
	if err != nil {
		clog.Warningf(ctx, "failed to write failure_summary: %v", err)
	}
}

func (b *Builder) outputFailedCommands(ctx context.Context, step *Step, err error) {
	if err == nil || b.failedCommandsWriter == nil || step.cmd == nil {
		return
	}
	var buf bytes.Buffer
	// rspfile is preserved when failed, so assume it exists.
	comment := "#"
	newline := "\n"
	if runtime.GOOS == "windows" {
		comment = "rem"
		newline = "\r\n"
	}
	fmt.Fprintf(&buf, "%s step_id=%s%s", comment, step.String(), newline)
	fmt.Fprintf(&buf, "%s %s%s", comment, step.cmd.Desc, newline)
	fmt.Fprintf(&buf, "%s%s", step.def.Binding("command"), newline)
	fmt.Fprint(&buf, newline)
	_, err = b.failedCommandsWriter.Write(buf.Bytes())
	if err != nil {
		clog.Warningf(ctx, "failed to write failed commands: %v", err)
	}
}
