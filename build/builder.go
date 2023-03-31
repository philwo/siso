// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	log "github.com/golang/glog"

	"infra/build/siso/execute"
	"infra/build/siso/execute/localexec"
	"infra/build/siso/execute/remoteexec"
	"infra/build/siso/hashfs"
	"infra/build/siso/o11y/clog"
	"infra/build/siso/o11y/trace"
	"infra/build/siso/sync/semaphore"
	"infra/build/siso/ui"
)

// OutputLocalFunc is a function to determine the file should be downloaded or not.
type OutputLocalFunc func(context.Context, string) bool

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
	// build session id, tool invocation id.
	id string

	progress progress

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

	preprocSema *semaphore.Semaphore

	localSema *semaphore.Semaphore
	localExec localexec.LocalExec

	remoteSema        *semaphore.Semaphore
	remoteExec        *remoteexec.RemoteExec
	reCacheEnableRead bool
	// TODO(b/266518906): enable reCacheEnableWrite option for read-only client.
	// reCacheEnableWrite bool

	actionSalt []byte

	sharedDepsLog SharedDepsLog

	outputLocal OutputLocalFunc

	cacheSema *semaphore.Semaphore
	cache     *Cache

	localexecLogWriter io.Writer

	clobber bool
	dryRun  bool
}

// dedupInputs deduplicates inputs.
// For windows worker, which uses case insensitive file system, it also
// deduplicates filenames with different cases, e.g. "Windows.h" vs "windows.h".
// TODO(b/275452106): support Mac worker
func dedupInputs(ctx context.Context, cmd *execute.Cmd) {
	// need to dedup input with different case in intermediate dir on win and mac?
	caseInsensitive := cmd.Platform["OSFamily"] == "Windows"
	m := make(map[string]string)
	inputs := make([]string, 0, len(cmd.Inputs))
	for _, input := range cmd.Inputs {
		key := input
		if caseInsensitive {
			key = strings.ToLower(input)
		}
		if s, found := m[key]; found {
			if log.V(1) {
				clog.Infof(ctx, "dedup input %s (%s)", input, s)
			}
			continue
		}
		m[key] = input
		inputs = append(inputs, input)
	}
	cmd.Inputs = make([]string, len(inputs))
	copy(cmd.Inputs, inputs)
}

// outputs processes step's outputs.
// it will flush outputs to local disk if
// - it is specified in local outputs of StepDef.
// - it has an extension that requires scan deps of future steps.
// - it is specified by OutptutLocalFunc.
func (b *Builder) outputs(ctx context.Context, step *Step) error {
	ctx, span := trace.NewSpan(ctx, "outputs")
	defer span.Close(nil)
	span.SetAttr("outputs", len(step.cmd.Outputs))
	localOutputs := step.def.LocalOutputs()
	span.SetAttr("outputs-local", len(localOutputs))
	seen := make(map[string]bool)
	for _, o := range localOutputs {
		if seen[o] {
			continue
		}
		seen[o] = true
	}

	clog.Infof(ctx, "outputs %d->%d", len(step.cmd.Outputs), len(localOutputs))
	allowMissing := step.def.Binding("allow_missing_outputs") != ""
	// need to check against step.cmd.Outputs, not step.def.Outputs, since
	// handler may add to step.cmd.Outputs.
	for _, out := range step.cmd.Outputs {
		// force to output local for inputs
		// .h,/.hxx/.hpp/.inc/.c/.cc/.cxx/.cpp/.m/.mm for gcc deps or msvc showIncludes
		// .json/.js/.ts for tsconfig.json, .js for grit etc.
		switch filepath.Ext(out) {
		case ".h", ".hxx", ".hpp", ".inc", ".c", ".cc", "cxx", ".cpp", ".m", ".mm", ".json", ".js", ".ts":
			if seen[out] {
				continue
			}
			localOutputs = append(localOutputs, out)
			seen[out] = true
		}
		if b.outputLocal != nil && b.outputLocal(ctx, out) {
			if seen[out] {
				continue
			}
			localOutputs = append(localOutputs, out)
			seen[out] = true
		}
		_, err := b.hashFS.Stat(ctx, step.cmd.ExecRoot, out)
		if err != nil {
			if allowMissing {
				clog.Warningf(ctx, "missing outputs %s: %v", out, err)
				outs := make([]string, 0, len(localOutputs))
				for _, f := range localOutputs {
					if f == out {
						continue
					}
					outs = append(outs, f)
				}
				localOutputs = outs
				continue
			}
			return fmt.Errorf("missing outputs %s: %w", out, err)
		}
	}
	if len(localOutputs) > 0 {
		err := b.hashFS.Flush(ctx, step.cmd.ExecRoot, localOutputs)
		if err != nil {
			return fmt.Errorf("failed to flush outputs to local: %w", err)
		}
	}
	return nil
}

// progressStepCacheHit shows progress of the cache hit step.
func (b *Builder) progressStepCacheHit(ctx context.Context, step *Step) {
	b.progress.step(ctx, b, step, "c "+step.cmd.Desc)
}

// progressStepCacheHit shows progress of the skipped step.
func (b *Builder) progressStepSkipped(ctx context.Context, step *Step) {
	b.progress.step(ctx, b, step, "- "+step.cmd.Desc)
}

// progressStepCacheHit shows progress of the started step.
func (b *Builder) progressStepStarted(ctx context.Context, step *Step) {
	step.SetPhase(stepStart)
	step.startTime = time.Now()
	b.progress.step(ctx, b, step, "S "+step.cmd.Desc)
}

// progressStepCacheHit shows progress of the finished step.
func (b *Builder) progressStepFinished(ctx context.Context, step *Step) {
	step.SetPhase(stepDone)
	b.progress.step(ctx, b, step, "F "+step.cmd.Desc)
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

func (b *Builder) phonyDone(ctx context.Context, step *Step) error {
	if log.V(1) {
		clog.Infof(ctx, "step phony %s", step)
	}
	b.plan.done(ctx, step, step.def.Outputs())
	return nil
}

func (b *Builder) done(ctx context.Context, step *Step) error {
	ctx, span := trace.NewSpan(ctx, "done")
	defer span.Close(nil)
	var outputs []string
	allowMissing := step.def.Binding("allow_missing_outputs") != ""
	for _, out := range step.cmd.Outputs {
		out := out
		var mtime time.Time
		if log.V(1) {
			clog.Infof(ctx, "output -> %s", out)
		}
		fi, err := b.hashFS.Stat(ctx, step.cmd.ExecRoot, out)
		if err != nil {
			if allowMissing {
				clog.Warningf(ctx, "missing output %s: %v", out, err)
				continue
			}
			if !b.dryRun {
				return fmt.Errorf("output %s for %s: %w", out, step, err)
			}
		}
		if fi != nil {
			mtime = fi.ModTime()
		}
		if log.V(1) {
			clog.Infof(ctx, "become ready: %s %s", out, mtime)
		}
		outputs = append(outputs, out)
	}
	b.stats.done(step.cmd.Pure)
	b.plan.done(ctx, step, outputs)
	return nil
}

func (b *Builder) failedToRun(ctx context.Context, cmd *execute.Cmd, err error) {
	clog.Warningf(ctx, "Failed to exec: %v", err)
	if errors.Is(err, context.Canceled) || ctx.Err() != nil {
		return
	}
	var output string
	if len(cmd.Outputs) > 0 {
		output = cmd.Outputs[0]
		if strings.HasPrefix(output, cmd.Dir+"/") {
			output = "./" + strings.TrimPrefix(output, cmd.Dir+"/")
		}
	}
	var msgs []string
	msgs = append(msgs, "\n", fmt.Sprintf("\nFAILED: %s %s\n%q %q\n%q\n", cmd, cmd.Desc, cmd.ActionName, output, cmd.Command()))
	rsp := cmd.RSPFile
	if rsp != "" {
		msgs = append(msgs, fmt.Sprintf(" %s=%q\n", rsp, cmd.RSPFileContent))
	}
	ui.PrintLines(msgs...)
	os.Stdout.Write(cmd.Stdout())
	os.Stderr.Write(append(cmd.Stderr(), '\n'))
}
