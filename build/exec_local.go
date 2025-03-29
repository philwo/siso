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
)

func (b *Builder) execLocal(ctx context.Context, step *Step) error {
	step.cmd.RemoteWrapper = ""

	step.setPhase(stepInput)
	err := b.prepareLocalInputs(ctx, step)
	if err != nil {
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

	queueTime := time.Now()
	step.setPhase(phase.wait())

	if err = sema.Acquire(ctx, 1); err != nil {
		return err
	}
	step.setPhase(phase)
	if step.cmd.Console {
		b.progress.startConsoleCmd(step.cmd)
	}
	started := time.Now()
	err = b.localExec.Run(ctx, step.cmd)
	dur := time.Since(started)
	step.setPhase(stepOutput)
	if step.cmd.Console {
		b.progress.finishConsoleCmd()
	}
	result := step.cmd.ActionResult()
	if result != nil {
		if result.ExecutionMetadata == nil {
			result.ExecutionMetadata = &rpb.ExecutedActionMetadata{}
		}
		result.ExecutionMetadata.QueuedTimestamp = timestamppb.New(queueTime)
		result.ExecutionMetadata.WorkerStartTimestamp = timestamppb.New(started)
	}
	sema.Release(1)

	if !errors.Is(err, context.Canceled) {
		b.logLocalExec(step, dur)
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
	err := b.hashFS.Flush(ctx, step.cmd.ExecRoot, inputs)
	// now, all inputs are expected to be on disk.
	// for local, no need to scan deps.
	inputs = b.hashFS.Availables(ctx, step.cmd.ExecRoot, step.cmd.Inputs)
	if len(inputs) != len(step.cmd.Inputs) {
		step.cmd.Inputs = inputs
	}
	return err
}

// checkLocalOutputs checks if all outputs are on local disk.
// If not, it returns error.
// It ignores missing outputs added by siso config.
func (b *Builder) checkLocalOutputs(ctx context.Context, step *Step) error {
	result := step.cmd.ActionResult()
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

func (b *Builder) logLocalExec(step *Step, dur time.Duration) {
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
}
