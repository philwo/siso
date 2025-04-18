// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/log"
	"go.chromium.org/infra/build/siso/execute"
	"go.chromium.org/infra/build/siso/reapi/merkletree"
	"go.chromium.org/infra/build/siso/scandeps"
	"go.chromium.org/infra/build/siso/toolsupport/gccutil"
	"go.chromium.org/infra/build/siso/toolsupport/makeutil"
)

type depsGCC struct {
	// for unittest
	treeInput func(context.Context, string) (merkletree.TreeEntry, error)
}

func (gcc depsGCC) fixCmdInputs(ctx context.Context, b *Builder, cmd *execute.Cmd) ([]string, error) {
	params := gccutil.ExtractScanDepsParams(cmd.Args, cmd.Env)
	for i := range params.Files {
		params.Files[i] = b.path.MaybeFromWD(params.Files[i])
	}
	for i := range params.Dirs {
		params.Dirs[i] = b.path.MaybeFromWD(params.Dirs[i])
	}
	for i := range params.Frameworks {
		params.Frameworks[i] = b.path.MaybeFromWD(params.Frameworks[i])
	}
	for i := range params.Sysroots {
		params.Sysroots[i] = b.path.MaybeFromWD(params.Sysroots[i])
	}
	var inputs []string
	// include files detected by command line. i.e. sanitaizer ignore lists.
	// These would not be in depsfile, different from Sources.
	inputs = append(inputs, params.Files...)
	// include directory must be included, even if no include files there.
	// without the dir, it may fail for `#include "../config.h"`
	inputs = append(inputs, params.Dirs...)
	// also frameworks include dirs.
	inputs = append(inputs, params.Frameworks...)
	// sysroot directory must be included, even if no include files there.
	// or error with
	// clang++: error: no such sysroot directory: ...
	// [-Werror, -Wmissing-sysroot]
	inputs = append(inputs, params.Sysroots...)
	inputs = b.expandInputs(ctx, inputs)

	fn := func(ctx context.Context, dir string) (merkletree.TreeEntry, error) {
		return b.treeInput(ctx, dir, ":headers", nil)
	}
	if gcc.treeInput != nil {
		fn = gcc.treeInput
	}
	precomputedDirs := make([]string, 0, len(params.Sysroots)+len(params.Frameworks))
	precomputedDirs = append(precomputedDirs, params.Sysroots...)
	precomputedDirs = append(precomputedDirs, params.Frameworks...)
	cmd.TreeInputs = append(cmd.TreeInputs, treeInputs(ctx, fn, precomputedDirs, params.Dirs)...)
	return inputs, nil
}

// TODO: use handler?
func (depsGCC) fixForSplitDwarf(cmd *execute.Cmd) {
	hasSplitDwarf := false
	for _, arg := range cmd.Args {
		if arg == "-gsplit-dwarf" {
			hasSplitDwarf = true
			break
		}
	}
	if !hasSplitDwarf {
		return
	}
	dwo := ""
	for _, out := range cmd.Outputs {
		if strings.HasSuffix(out, ".o") { // TODO: or ".obj" for win?
			dwo = strings.TrimSuffix(out, ".o") + ".dwo"
			continue
		}
	}
	cmd.Outputs = uniqueFiles(cmd.Outputs, []string{dwo})
}

func (depsGCC) DepsAfterRun(ctx context.Context, b *Builder, step *Step) ([]string, error) {
	if step.cmd.Deps != "gcc" {
		return nil, fmt.Errorf("gcc-deps; unexpected deps=%q %s", step.cmd.Deps, step)
	}
	defer func() {
		// don't remove depfile if it is used as output.
		for _, out := range step.cmd.Outputs {
			if out == step.cmd.Depfile {
				return
			}
		}
		b.hashFS.Remove(ctx, step.cmd.ExecRoot, step.cmd.Depfile)
		b.hashFS.Flush(ctx, step.cmd.ExecRoot, []string{step.cmd.Depfile})
	}()
	buf, err := b.hashFS.ReadFile(ctx, step.cmd.ExecRoot, step.cmd.Depfile)
	if err != nil {
		return nil, fmt.Errorf("gcc-deps: failed to get depfile %q of %s: %w", step.cmd.Depfile, step, err)
	}

	deps, err := makeutil.ParseDeps(buf)
	if err != nil {
		return nil, fmt.Errorf("gcc-deps: failed to parse depfile %q: %w", step.cmd.Depfile, err)
	}
	err = checkDeps(ctx, b, step, deps)
	if err != nil {
		return nil, fmt.Errorf("error in depfile %q: %w", step.cmd.Depfile, err)
	}
	return deps, nil
}

func (gcc depsGCC) DepsCmd(ctx context.Context, b *Builder, step *Step) ([]string, error) {
	depsIns, err := gcc.depsInputs(ctx, b, step)
	if err != nil {
		return nil, err
	}
	if b.reapiclient != nil {
		inputs, err := gcc.fixCmdInputs(ctx, b, step.cmd)
		if err != nil {
			return nil, err
		}
		depsIns = append(depsIns, inputs...)
	}
	gcc.fixForSplitDwarf(step.cmd)
	return depsIns, err
}

func (gcc depsGCC) depsInputs(ctx context.Context, b *Builder, step *Step) ([]string, error) {
	return gcc.scandeps(ctx, b, step)
}

func (depsGCC) scandeps(ctx context.Context, b *Builder, step *Step) ([]string, error) {
	params := gccutil.ExtractScanDepsParams(step.cmd.Args, step.cmd.Env)
	for i := range params.Sources {
		params.Sources[i] = b.path.MaybeFromWD(params.Sources[i])
	}
	for i := range params.Includes {
		params.Includes[i] = b.path.MaybeFromWD(params.Includes[i])
	}
	for i := range params.Files {
		params.Files[i] = b.path.MaybeFromWD(params.Files[i])
	}
	for i := range params.Dirs {
		params.Dirs[i] = b.path.MaybeFromWD(params.Dirs[i])
	}
	for i := range params.Frameworks {
		params.Frameworks[i] = b.path.MaybeFromWD(params.Frameworks[i])
	}
	for i := range params.Sysroots {
		params.Sysroots[i] = b.path.MaybeFromWD(params.Sysroots[i])
	}
	req := scandeps.Request{
		Defines:    params.Defines,
		Sources:    params.Sources,
		Includes:   params.Includes,
		Dirs:       params.Dirs,
		Frameworks: params.Frameworks,
		Sysroots:   params.Sysroots,
		Timeout:    step.cmd.Timeout,
	}
	ins, err := b.scanDeps.Scan(ctx, b.path.ExecRoot, req)
	if err != nil {
		buf, berr := json.Marshal(req)
		log.Warnf("scandeps failed Request %s %v: %v", buf, berr, err)
		return nil, err
	}
	ins = append(ins, params.Files...)
	for i := range ins {
		ins[i] = b.path.Intern(ins[i])
	}
	return ins, nil
}
