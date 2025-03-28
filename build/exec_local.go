// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/charmbracelet/log"
	"google.golang.org/protobuf/types/known/timestamppb"

	"go.chromium.org/infra/build/siso/execute"
	"go.chromium.org/infra/build/siso/reapi/digest"
)

func (b *Builder) execLocal(ctx context.Context, step *Step) error {
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
	var executor execute.Executor = b.localExec
	logLocalExec := b.logLocalExec
	switch sandbox := step.def.Binding("sandbox"); sandbox {
	// TODO(crbug.com/420752996): add sandbox supports
	case "":
		enableTrace := experiments.Enabled("file-access-trace", "enable file-access-trace")
		if enableTrace {
			// check impure explicitly set in config,
			// rather than step.cmd.Pure.
			// step.cmd.Pure may be false when config is not set
			// for the step too, but we want to disable
			// file-access-trace only for the step with impure=true.
			// http://b/261655377 errorprone_plugin_tests: too slow under strace?
			impure := step.def.Binding("impure") == "true"
			if impure {
				log.Warnf("disable file-access-trace by impure")
			} else {
				traceExecutor, err := newFileTraceExecutor(b, executor)
				if err != nil {
					return fmt.Errorf("unable to perform file-access-trace: %w", err)
				}
				executor = traceExecutor
				logLocalExec = traceExecutor.logLocalExec
			}
		}
	default:
		log.Warnf("unsupported sandbox %q", sandbox)
	}

	if phase == stepLocalRun && step.metrics.Fallback {
		phase = stepFallbackRun
		stateMessage = "local exec [fallback]"
	}

	queueTime := time.Now()
	var dur time.Duration
	step.setPhase(phase.wait())
	err = sema.Do(ctx, func() error {
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
		err := executor.Run(ctx, step.cmd)
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
		step.metrics.done(step, b.start)
		return err
	})
	if !errors.Is(err, context.Canceled) {
		lerr := logLocalExec(ctx, step, dur)
		if err == nil {
			err = lerr
		}
	}
	if err != nil {
		return err
	}
	err = b.trustedLocalUpload(ctx, step)
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

// Uploads and sets local execution result in RE if builder is trusted
// Note: currently does not work with layered cache and blocks on digest calculation
func (b *Builder) trustedLocalUpload(ctx context.Context, step *Step) error {
	// Local upload must be enabled and step must have pure inputs/outputs
	if b.reapiclient == nil || !b.reCacheEnableWrite || !step.cmd.Pure {
		return nil
	}
	// Action digests are lazily computed for local so they are not available at this point
	cmd := step.cmd
	result, _ := cmd.ActionResult()
	ds := digest.NewStore()
	actionDigest, err := cmd.Digest(ctx, ds)

	if err != nil {
		log.Warnf("failed to compute digest for trusted local upload: %v", err)
		return err
	}

	// Create new ActionResult to not mutate cmd result
	// We need to unset StderrRaw, StdoutRaw, and populate OutputFiles
	result = &rpb.ActionResult{
		OutputFiles:       result.GetOutputFiles(),
		OutputSymlinks:    result.GetOutputSymlinks(),
		OutputDirectories: result.GetOutputDirectories(),
		ExitCode:          result.GetExitCode(),
		StdoutRaw:         result.GetStdoutRaw(),
		StderrRaw:         result.GetStderrRaw(),
		StdoutDigest:      result.GetStdoutDigest(),
		StderrDigest:      result.GetStderrDigest(),
		ExecutionMetadata: result.GetExecutionMetadata(),
	}

	// Retrieve and compute output digests from HashFS on the action
	hashFS := b.hashFS
	outputEntries, err := hashFS.Entries(ctx, cmd.ExecRoot, cmd.AllOutputs())
	if err != nil {
		return err
	}

	// Convert rawStdout to digest since RE spec v2 prohibits inlining
	if len(result.GetStdoutRaw()) != 0 && result.GetStdoutDigest() == nil {
		stdoutDigest := digest.FromBytes("stdout", result.GetStdoutRaw())
		result.StdoutDigest = stdoutDigest.Digest().Proto()
		ds.Set(stdoutDigest)
	}
	result.StdoutRaw = nil

	// Convert rawStderr to digest since RE spec v2 prohibits inlining
	if len(result.GetStderrRaw()) != 0 && result.GetStderrDigest() == nil {
		stderrDigest := digest.FromBytes("stderr", result.GetStderrRaw())
		result.StderrDigest = stderrDigest.Digest().Proto()
		ds.Set(stderrDigest)
	}
	result.StderrRaw = nil

	// Set the outputs on the result
	execute.ResultFromEntries(ctx, result, cmd.Dir, outputEntries)
	for _, entry := range outputEntries {
		ds.Set(entry.Data)
	}

	// Upload all collected output data, input data, and action itself
	_, err = b.reapiclient.UploadAll(ctx, ds)
	if err != nil {
		return err
	}
	// Now set the action result in RE
	return b.reapiclient.UpdateActionResult(ctx, actionDigest, result)
}

func (b *Builder) prepareLocalInputs(ctx context.Context, step *Step) error {
	inputs := step.cmd.AllInputs()
	err := b.hashFS.Flush(ctx, step.cmd.ExecRoot, inputs)
	// now, all inputs are expected to be on disk.
	// for local, no need to scan deps.
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
		return nil
	}

	defOutputs := step.def.Outputs(ctx)

	for _, out := range step.cmd.Outputs {
		_, err := step.cmd.HashFS.Stat(ctx, step.cmd.ExecRoot, out)
		if err != nil {
			required := slices.Contains(defOutputs, out)
			if !required {
				log.Warnf("ignore missing outputs %s: %v", out, err)
				continue
			}
			return fmt.Errorf("missing outputs %s: %w", out, err)
		}
	}
	// don't set result.OutputFiles etc to lazily calculate digest
	// for outputs. b/311312613
	return nil
}

func (b *Builder) logLocalExec(ctx context.Context, step *Step, dur time.Duration) error {
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
		log.Warnf("failed to log localexec: %v", err)
	}
	return nil
}
