// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"path/filepath"
	"time"

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

	localSema *semaphore.Semaphore
	localExec localexec.LocalExec

	remoteSema        *semaphore.Semaphore
	reCacheEnableRead bool
	// TODO(b/266518906): enable reCacheEnableWrite option for read-only client.
	// reCacheEnableWrite bool

	actionSalt []byte

	sharedDepsLog SharedDepsLog

	localexecLogWriter io.Writer

	clobber bool
}

func (b *Builder) skipped(ctx context.Context, step *Step) {
	b.progress.step(ctx, b, step, "- "+step.cmd.Desc)
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
