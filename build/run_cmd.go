// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
)

func (b *Builder) runStrategy(step *Step) func(context.Context, *Step) error {
	// Criteria for remote executable:
	// - Remote execution is available
	// - Command has platform container-image property
	// If the command doesn't meet all criteria, fallback to local.
	// Any further validation should be done in the exec handler, not here.
	allowRemote := b.remoteExec != nil && len(step.cmd.Platform) > 0 && step.cmd.Platform["container-image"] != ""
	switch {
	case step.cmd.Pure && allowRemote:
		return b.runRemote
	default:
		return b.runLocal
	}
}

func (b *Builder) runLocal(ctx context.Context, step *Step) error {
	// preproc performs scandeps to list up all inputs, so
	// we can flush these inputs before local execution.
	// but we already flushed generated *.h etc, no need to
	// preproc for local run.
	dedupInputs(step.cmd)
	// TODO: use local cache?
	return b.execLocal(ctx, step)
}
