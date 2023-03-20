// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	log "github.com/golang/glog"
	tspb "google.golang.org/protobuf/types/known/timestamppb"

	"infra/build/siso/execute"
	"infra/build/siso/execute/localexec"
	"infra/build/siso/hashfs"
	"infra/build/siso/o11y/clog"
	"infra/build/siso/o11y/trace"
	"infra/build/siso/sync/semaphore"
)

// logging labels's key.
const (
	logLabelKeyID        = "id"
	logLabelKeyBacktrace = "backtrace"
)

var experiments Experiments

// Metadata is a metadata of the build process.
type Metadata struct {
	// KV is an arbitrary key-value pair in the metadata.
	KV     map[string]string
	NumCPU int
	GOOS   string
	GOARCH string
	// want to include hostname?
}

// Builder is a builder.
type Builder struct {
	// path system used in the build.
	path   *Path
	hashFS *hashfs.HashFS

	// arg table to intern command line args of steps.
	argTab symtab

	start time.Time
	graph Graph
	plan  *plan
	stats *stats

	stepSema *semaphore.Semaphore

	localSema *semaphore.Semaphore
	localExec localexec.LocalExec

	remoteSema        *semaphore.Semaphore
	reCacheEnableRead bool
	// TODO(b/266518906): enable reCacheEnableWrite option for read-only client.
	// reCacheEnableWrite bool

	actionSalt []byte

	sharedDepsLog SharedDepsLog

	localexecLogWriter io.Writer
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
		err := b.hashFS.Mkdir(ctx, b.path.ExecRoot, outdir)
		if err != nil {
			return fmt.Errorf("prepare outdirs %s: %w", outdir, err)
		}
		seen[outdir] = true
	}
	b.hashFS.Forget(ctx, b.path.ExecRoot, step.cmd.Outputs)
	return nil
}

var errNotRelocatable = errors.New("request is not relocatable")

func (b *Builder) updateDeps(ctx context.Context, step *Step) error {
	ctx, span := trace.NewSpan(ctx, "update-deps")
	defer span.Close(nil)
	if len(step.cmd.Outputs) == 0 {
		clog.Warningf(ctx, "update deps: no outputs")
		return nil
	}
	output, err := filepath.Rel(step.cmd.Dir, step.cmd.Outputs[0])
	if err != nil {
		clog.Warningf(ctx, "update deps: failed to get rel %s,%s: %v", step.cmd.Dir, step.cmd.Outputs[0], err)
		return nil
	}
	fi, err := b.hashFS.Stat(ctx, step.cmd.ExecRoot, step.cmd.Outputs[0])
	if err != nil {
		clog.Warningf(ctx, "update deps: missing outputs %s: %v", step.cmd.Outputs[0], err)
		return nil
	}
	deps, err := depsAfterRun(ctx, b, step)
	if err != nil {
		clog.Warningf(ctx, "update deps: %v", err)
		return err
	}
	if len(deps) == 0 {
		return nil
	}
	var updated bool
	if step.fastDeps {
		// if fastDeps case, we already know the correct deps for this cmd.
		// just update for local deps log for incremental build.
		updated, err = step.def.RecordDeps(ctx, output, fi.ModTime(), deps)
	} else {
		// otherwise, update both local and shared.
		updated, err = b.recordDepsLog(ctx, step.def, output, step.cmd.CmdHash, fi.ModTime(), deps)
	}
	if err != nil {
		clog.Warningf(ctx, "update deps: failed to record deps %s, %s, %s, %s: %v", output, hex.EncodeToString(step.cmd.CmdHash), fi.ModTime(), deps, err)
	}
	clog.Infof(ctx, "update deps=%s: %s %s %d updated:%t pure:%t/%t->true", step.cmd.Deps, output, hex.EncodeToString(step.cmd.CmdHash), len(deps), updated, step.cmd.Pure, step.cmd.Pure)
	span.SetAttr("deps", len(deps))
	span.SetAttr("updated", updated)
	for i := range deps {
		deps[i] = b.path.MustFromWD(deps[i])
	}
	depsFixCmd(ctx, b, step, deps)
	return nil
}

func (b *Builder) captureLocalOutputs(ctx context.Context, step *Step) error {
	ctx, span := trace.NewSpan(ctx, "capture-local-outputs")
	defer span.Close(nil)
	span.SetAttr("outputs", len(step.cmd.Outputs))
	result := step.cmd.ActionResult()
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

func (b *Builder) runLocal(ctx context.Context, step *Step) error {
	ctx, span := trace.NewSpan(ctx, "run-local")
	defer span.Close(nil)
	clog.Infof(ctx, "run local %s", step.cmd.Desc)
	step.cmd.RemoteWrapper = ""

	step.SetPhase(stepInput)
	// expand inputs to get full action inputs,
	// before preparing inputs on local disk for local action.
	depsExpandInputs(ctx, b, step)
	err := b.prepareLocalInputs(ctx, step)
	if err != nil && !experiments.Enabled("ignore-missing-local-inputs", "step %s missing inputs: %v", step, err) {
		return err
	}
	err = b.prepareLocalOutdirs(ctx, step)
	if err != nil {
		return err
	}
	if localexec.TraceEnabled(ctx) {
		// check impure explicitly set in config,
		// rather than step.cmd.Pure.
		// step.cmd.Pure may be false when config is not set
		// for the step too, but we want to disable
		// file-access-trace only for the step with impure=true.
		// http://b/261655377 errorprone_plugin_tests: too slow under strace?
		impure := step.def.Binding("impure") == "true"
		x := experiments.Enabled("no-file-access-trace", "disabled file-access-trace")
		if !impure && !x {
			step.cmd.FileTrace = &execute.FileTrace{}
		} else {
			clog.Warningf(ctx, "disable file-access-trace impure=%t no-file-access-trace=%t", impure, x)
		}
	}
	queueTime := time.Now()
	var dur time.Duration
	err = b.localSema.Do(ctx, func(ctx context.Context) error {
		clog.Infof(ctx, "step state: local exec")
		step.SetPhase(stepLocalRun)
		started := time.Now()
		err := b.localExec.Run(ctx, step.cmd)
		dur = time.Since(started)
		step.SetPhase(stepOutput)
		b.stats.localDone(ctx, err)
		if step.cmd.ActionResult() != nil {
			if step.cmd.ActionResult().ExecutionMetadata == nil {
				step.cmd.ActionResult().ExecutionMetadata = &rpb.ExecutedActionMetadata{}
			}
			step.cmd.ActionResult().ExecutionMetadata.QueuedTimestamp = tspb.New(queueTime)
			step.cmd.ActionResult().ExecutionMetadata.WorkerStartTimestamp = tspb.New(started)
		}
		step.metrics.RunTime = IntervalMetric(time.Since(started))
		step.metrics.done(ctx, step)
		return err
	})
	if err != nil {
		return err
	}
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
	return nil
}

func argsForLogLocalExec(cmdArgs []string) []string {
	var args []string
	if len(cmdArgs) > 10 {
		args = append(args, cmdArgs[:10]...)
		args = append(args, "...")
	} else {
		args = append(args, cmdArgs...)
	}
	return args
}

func (b *Builder) logLocalExec(ctx context.Context, step *Step, dur time.Duration) {
	args := argsForLogLocalExec(step.cmd.Args)
	allOutputs := step.cmd.AllOutputs()
	var output string
	if len(allOutputs) > 0 {
		output = allOutputs[0]
	}
	var buf bytes.Buffer
	fmt.Fprintf(&buf, `cmd: %s pure:%t/unknown restat:%t %s
action: %s %s
args: %q %d

`,
		step, step.cmd.Pure, step.cmd.Restat, dur,
		step.cmd.ActionName, output,
		args, dur.Milliseconds())
	_, err := b.localexecLogWriter.Write(buf.Bytes())
	if err != nil {
		clog.Warningf(ctx, "failed to log localexec: %v", err)
	}
}

func (b *Builder) checkTrace(ctx context.Context, step *Step, dur time.Duration) error {
	// TODO(b/266518906): migrate from infra_internal/experimental
	return nil
}
