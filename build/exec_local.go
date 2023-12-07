// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	log "github.com/golang/glog"
	"google.golang.org/protobuf/types/known/timestamppb"

	"infra/build/siso/execute"
	"infra/build/siso/execute/localexec"
	"infra/build/siso/o11y/clog"
	"infra/build/siso/o11y/trace"
)

func (b *Builder) execLocal(ctx context.Context, step *Step) error {
	ctx, span := trace.NewSpan(ctx, "exec-local")
	defer span.Close(nil)
	clog.Infof(ctx, "exec local %s", step.cmd.Desc)
	step.cmd.RemoteWrapper = ""

	step.setPhase(stepInput)
	// expand inputs to get full action inputs,
	// before preparing inputs on local disk for local action.
	depsExpandInputs(ctx, b, step)
	b.fixMissingInputs(ctx, step)
	err := b.prepareLocalInputs(ctx, step)
	if err != nil && !experiments.Enabled("ignore-missing-local-inputs", "step %s missing inputs: %v", step, err) {
		return err
	}
	err = b.prepareLocalOutdirs(ctx, step)
	if err != nil {
		return err
	}
	_, err = b.prevOutputEntries(ctx, step)
	if err != nil {
		clog.Warningf(ctx, "failed to get entries for prev outputs: %v", err)
	}

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

	queueTime := time.Now()
	var dur time.Duration
	err = sema.Do(ctx, func(ctx context.Context) error {
		clog.Infof(ctx, "step state: %s", stateMessage)
		step.setPhase(phase)
		started := time.Now()
		step.metrics.ActionStartTime = IntervalMetric(started.Sub(b.start))
		err := b.localExec.Run(ctx, step.cmd)
		dur = time.Since(started)
		step.setPhase(stepOutput)
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
		step.metrics.done(ctx, step)
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
	err = b.captureLocalOutputs(ctx, step)
	if err != nil {
		return err
	}
	return b.outputs(ctx, step)
}

func (b *Builder) prepareLocalInputs(ctx context.Context, step *Step) error {
	ctx, span := trace.NewSpan(ctx, "prepare-local-inputs")
	defer span.Close(nil)
	inputs := step.cmd.AllInputs()
	span.SetAttr("inputs", len(inputs))
	start := time.Now()
	if log.V(1) {
		clog.Infof(ctx, "prepare-local-inputs %d", len(inputs))
	}
	err := b.hashFS.Flush(ctx, step.cmd.ExecRoot, inputs)
	clog.Infof(ctx, "prepare-local-inputs %d %s: %v", len(inputs), time.Since(start), err)
	return err
}

func (b *Builder) prepareLocalOutdirs(ctx context.Context, step *Step) error {
	ctx, span := trace.NewSpan(ctx, "prepare-local-outdirs")
	defer span.Close(nil)

	seen := make(map[string]bool)
	for _, out := range step.cmd.Outputs {
		outdir := filepath.Dir(out)
		if seen[outdir] {
			continue
		}
		clog.Infof(ctx, "prepare outdirs %s", outdir)
		err := b.hashFS.Mkdir(ctx, b.path.ExecRoot, outdir, nil)
		if err != nil {
			return fmt.Errorf("prepare outdirs %s: %w", outdir, err)
		}
		seen[outdir] = true
	}
	b.hashFS.Forget(ctx, b.path.ExecRoot, step.cmd.Outputs)
	return nil
}

func (b *Builder) captureLocalOutputs(ctx context.Context, step *Step) error {
	ctx, span := trace.NewSpan(ctx, "capture-local-outputs")
	defer span.Close(nil)
	span.SetAttr("outputs", len(step.cmd.Outputs))
	result, _ := step.cmd.ActionResult()
	if result.GetExitCode() != 0 {
		return nil
	}
	entries, err := step.cmd.HashFS.Entries(ctx, step.cmd.ExecRoot, step.cmd.Outputs)
	if err != nil {
		return fmt.Errorf("failed to get output fs entries %s: %w", step, err)
	}
	for _, entry := range entries {
		switch {
		case entry.IsDir():
			clog.Warningf(ctx, "unexpected output directory %s", entry.Name)
		case entry.IsSymlink():
			result.OutputFileSymlinks = append(result.OutputFileSymlinks, &rpb.OutputSymlink{
				Path:   entry.Name,
				Target: entry.Target,
			})
		default:
			result.OutputFiles = append(result.OutputFiles, &rpb.OutputFile{
				Path:         entry.Name,
				Digest:       entry.Data.Digest().Proto(),
				IsExecutable: entry.IsExecutable,
			})
		}
	}
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
