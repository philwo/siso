// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"errors"
	"time"

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

// Metadata is a metadata of the build process.
type Metadata struct {
	// KV is an arbitrary key-value pair in the metadata.
	KV     map[string]string
	NumCPU int
	GOOS   string
	GOARCH string
}
