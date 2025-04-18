// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"fmt"

	"github.com/charmbracelet/log"
	"go.chromium.org/infra/build/siso/toolsupport/makeutil"
)

type depsDepfile struct{}

func (depsDepfile) DepsAfterRun(ctx context.Context, b *Builder, step *Step) ([]string, error) {
	_, err := b.hashFS.Stat(ctx, b.path.ExecRoot, step.cmd.Depfile)
	if err != nil {
		return nil, err
	}
	fsys := b.hashFS.FileSystem(ctx, b.path.ExecRoot)
	depins, err := makeutil.ParseDepsFile(fsys, step.cmd.Depfile)
	if err != nil {
		return nil, fmt.Errorf("failed to parse depfile %q: %w", step.cmd.Depfile, err)
	}
	err = checkDeps(ctx, b, step, depins)
	if err != nil {
		return nil, fmt.Errorf("error in depfile %q: %w", step.cmd.Depfile, err)
	}
	log.Infof("depfile %s: %d", step.cmd.Depfile, len(depins))
	return depins, nil
}

func (depsDepfile) DepsCmd(ctx context.Context, b *Builder, step *Step) ([]string, error) {
	log.Infof("deps= depfile=%s. no pure, no remote", step.cmd.Depfile)
	step.cmd.Pure = false
	return step.cmd.Inputs, nil
}
