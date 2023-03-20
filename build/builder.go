// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	log "github.com/golang/glog"

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
}

var errNotRelocatable = errors.New("request is not relocatable")

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

	remoteSema        *semaphore.Semaphore
	reCacheEnableRead bool
	// TODO(b/266518906): enable reCacheEnableWrite option for read-only client.
	// reCacheEnableWrite bool

	actionSalt []byte

	sharedDepsLog SharedDepsLog
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
