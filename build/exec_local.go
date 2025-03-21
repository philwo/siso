// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	log "github.com/golang/glog"
	"google.golang.org/protobuf/types/known/timestamppb"

	"go.chromium.org/infra/build/siso/execute"
	"go.chromium.org/infra/build/siso/execute/localexec"
	"go.chromium.org/infra/build/siso/o11y/clog"
)

func (b *Builder) execLocal(ctx context.Context, step *Step) error {
	clog.Infof(ctx, "exec local %s", step.cmd.Desc)
	step.cmd.RemoteWrapper = ""

	step.setPhase(stepInput)
	err := b.prepareLocalInputs(ctx, step)
	if err != nil && !experiments.Enabled("ignore-missing-local-inputs", "step %s missing inputs: %v", step, err) {
		return err
	}
	step.cmd.RecordPreOutputs(ctx)

	stateMessage := "local exec"
	sema := b.localSema
	pool := step.def.Binding("pool")
	step.cmd.Console = pool == "console"
	if s, ok := b.poolSemas[pool]; ok {
		sema = s
		stateMessage += " (pool=" + pool + ")"
	}
	phase := stepLocalRun
	enableTrace := experiments.Enabled("file-access-trace", "enable file-access-trace")
	switch {
	// TODO(b/273407069): native integration instead of spwaning gomacc/rewrapper?
	case step.def.Binding("use_remote_exec_wrapper") != "":
		// no need to file trace for gomacc/rewwapper.
		stateMessage = "remote exec wrapper"
		phase = stepREWrapperRun
		sema = b.rewrapSema
	case localexec.TraceEnabled(ctx):
		// check impure explicitly set in config,
		// rather than step.cmd.Pure.
		// step.cmd.Pure may be false when config is not set
		// for the step too, but we want to disable
		// file-access-trace only for the step with impure=true.
		// http://b/261655377 errorprone_plugin_tests: too slow under strace?
		impure := step.def.Binding("impure") == "true"
		if !impure && enableTrace {
			step.cmd.FileTrace = &execute.FileTrace{}
		}
	case enableTrace:
		clog.Warningf(ctx, "unable to use file-access-trace")
	}
	if phase == stepLocalRun && step.metrics.Fallback {
		phase = stepFallbackRun
		stateMessage = "local exec [fallback]"
	}

	queueTime := time.Now()
	var dur time.Duration
	step.setPhase(phase.wait())
	err = sema.Do(ctx, func(ctx context.Context) error {
		clog.Infof(ctx, "step state: %s", stateMessage)
		step.setPhase(phase)
		if step.cmd.Console {
			b.progress.startConsoleCmd(step.cmd)
		}
		started := time.Now()
		// local exec might be called as fallback.
		// Do not change ActionStartTime if it's already set.
		if step.metrics.ActionStartTime == 0 {
			step.metrics.ActionStartTime = IntervalMetric(started.Sub(b.start))
		}
		err := b.localExec.Run(ctx, step.cmd)
		dur = time.Since(started)
		step.setPhase(stepOutput)
		if step.cmd.Console {
			b.progress.finishConsoleCmd()
		}
		step.metrics.IsLocal = true
		result, cached := step.cmd.ActionResult()
		if cached {
			step.metrics.Cached = true
		}
		if result != nil {
			if result.ExecutionMetadata == nil {
				result.ExecutionMetadata = &rpb.ExecutedActionMetadata{}
			}
			result.ExecutionMetadata.QueuedTimestamp = timestamppb.New(queueTime)
			result.ExecutionMetadata.WorkerStartTimestamp = timestamppb.New(started)
		}
		step.metrics.RunTime = IntervalMetric(time.Since(started))
		step.metrics.done(ctx, step, b.start)
		return err
	})
	if !errors.Is(err, context.Canceled) {
		if step.cmd.FileTrace != nil {
			cerr := b.checkTrace(ctx, step, dur)
			if cerr != nil {
				clog.Warningf(ctx, "failed to check trace %v", cerr)
				if err == nil {
					err = cerr
				}
			}
		} else {
			b.logLocalExec(ctx, step, dur)
		}
	}
	if err != nil {
		return err
	}
	err = b.updateDeps(ctx, step)
	if err != nil {
		return err
	}
	return b.checkLocalOutputs(ctx, step)
	// no need to call b.outputs, as all outputs are already on disk
	// so no need to flush.
}

func (b *Builder) prepareLocalInputs(ctx context.Context, step *Step) error {
	inputs := step.cmd.AllInputs()
	start := time.Now()
	if log.V(1) {
		clog.Infof(ctx, "prepare-local-inputs %d", len(inputs))
	}
	err := b.hashFS.Flush(ctx, step.cmd.ExecRoot, inputs)
	clog.Infof(ctx, "prepare-local-inputs %d %s: %v", len(inputs), time.Since(start), err)
	// now, all inputs are expected to be on disk.
	// for reproxy and local, no need to scan deps.
	// but need to remove missing inputs from cmd.Inputs
	// because we'll record header inputs for deps=msvc in deps log.
	// TODO: b/322712783 - minimize local disk check.
	if step.cmd.Deps == "msvc" {
		// we need to check this against local disk, not hashfs.
		// because command may add/remove files that are not
		// known in ninja build graph.
		inputs = b.hashFS.ForgetMissings(ctx, step.cmd.ExecRoot, step.cmd.Inputs)
	} else {
		// if deps is not "msvc", just check against hashfs.
		inputs = b.hashFS.Availables(ctx, step.cmd.ExecRoot, step.cmd.Inputs)
	}
	if len(inputs) != len(step.cmd.Inputs) {
		clog.Infof(ctx, "deps remove missing inputs %d -> %d", len(step.cmd.Inputs), len(inputs))
		step.cmd.Inputs = inputs
	}
	return err
}

// checkLocalOutputs checks if all outputs are on local disk.
// If not, it returns error.
// It ignores missing outputs added by siso config.
func (b *Builder) checkLocalOutputs(ctx context.Context, step *Step) error {
	result, _ := step.cmd.ActionResult()
	if result.GetExitCode() != 0 {
		return nil
	}
	if step.def.Binding("phony_output") != "" {
		clog.Infof(ctx, "phony_output. no check output files %q", step.cmd.Outputs)
		return nil
	}

	defOutputs := step.def.Outputs(ctx)

	for _, out := range step.cmd.Outputs {
		_, err := step.cmd.HashFS.Stat(ctx, step.cmd.ExecRoot, out)
		if err != nil {
			required := false
			for _, o := range defOutputs {
				if out == o {
					required = true
					break
				}
			}
			if !required {
				clog.Warningf(ctx, "ignore missing outputs %s: %v", out, err)
				continue
			}
			return fmt.Errorf("missing outputs %s: %w", out, err)
		}
	}
	// don't set result.OutputFiles etc to lazily calculate digest
	// for outputs. b/311312613
	return nil
}

func (b *Builder) logLocalExec(ctx context.Context, step *Step, dur time.Duration) {
	command := step.def.Binding("command")
	if len(command) > 256 {
		command = command[:256] + " ..."
	}
	allOutputs := step.cmd.AllOutputs()
	var output string
	if len(allOutputs) > 0 {
		output = allOutputs[0]
	}
	var buf bytes.Buffer
	fmt.Fprintf(&buf, `cmd: %s pure:%t/unknown restat:%t %s
action: %s %s
command: %q %d

`,
		step, step.cmd.Pure, step.cmd.Restat, dur,
		step.cmd.ActionName, output,
		command, dur.Milliseconds())
	_, err := b.localexecLogWriter.Write(buf.Bytes())
	if err != nil {
		clog.Warningf(ctx, "failed to log localexec: %v", err)
	}
}
