// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"

	log "github.com/golang/glog"

	"infra/build/siso/hashfs"
	"infra/build/siso/o11y/clog"
)

func (b *Builder) runStrategy(ctx context.Context, step *Step) func(context.Context, *Step) error {
	// Criteria for remote executable:
	// - Allow remote if available and command has platform container-image property.
	// - Allow reproxy if available and command has reproxy config set.
	// If the command doesn't meet either criteria, fallback to local.
	// Any further validation should be done in the exec handler, not here.
	allowRemote := b.remoteExec != nil && len(step.cmd.Platform) > 0 && step.cmd.Platform["container-image"] != ""
	allowREProxy := b.reproxyExec.Enabled() && step.cmd.REProxyConfig != nil
	switch {
	case step.cmd.Pure && allowREProxy:
		return b.runReproxy
	case step.cmd.Pure && allowRemote:
		return b.runRemote
	default:
		return b.runLocal
	}
}

func (b *Builder) runReproxy(ctx context.Context, step *Step) error {
	dedupInputs(ctx, step.cmd)
	// TODO: b/297807325 - Siso relies on Reproxy's local fallback for
	// monitoring at this moment. So, Siso shouldn't try local fallback.
	return b.execReproxy(ctx, step)
}

func (b *Builder) runLocal(ctx context.Context, step *Step) error {
	// preproc performs scandeps to list up all inputs, so
	// we can flush these inputs before local execution.
	// but we already flushed generated *.h etc, no need to
	// preproc for local run.
	dedupInputs(ctx, step.cmd)
	// TODO: use local cache?
	return b.execLocal(ctx, step)
}

func (b *Builder) fixMissingInputs(ctx context.Context, step *Step) {
	// for reproxy and local, no need to scan deps.
	// but need to remove missing inputs from cmd.Inputs
	// because we'll record header inputs for deps=msvc in deps log.
	inputs := make([]string, 0, len(step.cmd.Inputs))
	for _, in := range step.cmd.Inputs {
		if _, err := b.hashFS.Stat(ctx, b.path.ExecRoot, in); err == nil {
			inputs = append(inputs, in)
		} else if log.V(1) {
			clog.Infof(ctx, "remove missing inputs %s: %v", in, err)
		}
	}
	if len(inputs) != len(step.cmd.Inputs) {
		clog.Infof(ctx, "deps remove missing inputs %d -> %d", len(step.cmd.Inputs), len(inputs))
		step.cmd.Inputs = inputs
	}
}

// compute previous output entries before running command.
// hashfs would lazily compute digest of files, so it would
// cause ERROR_SHARING_VIOLATION when running command on Windows.
// to prevent the error, compute digest before running step.
// TODO: use this to enable restat for remote execution.
func (b *Builder) prevOutputEntries(ctx context.Context, step *Step) []hashfs.UpdateEntry {
	return b.hashFS.RetrieveUpdateEntries(ctx, b.path.ExecRoot, step.cmd.AllOutputs())
}
