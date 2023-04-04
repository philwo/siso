// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"fmt"
	"path/filepath"

	"infra/build/siso/execute"
	"infra/build/siso/o11y/clog"
	"infra/build/siso/o11y/trace"
	"infra/build/siso/toolsupport/msvcutil"
)

type depsMSVC struct{}

func (msvc depsMSVC) DepsFastCmd(ctx context.Context, b *Builder, cmd *execute.Cmd) (*execute.Cmd, error) {
	newCmd := &execute.Cmd{}
	*newCmd = *cmd
	msvc.fixArgsForDeps(newCmd)
	return newCmd, nil
}

func (depsMSVC) fixArgsForDeps(cmd *execute.Cmd) {
	// siso fast deps requires full dependency info.
	// /showIncludes:user only generates user dependency.
	for i, arg := range cmd.Args {
		if arg == "/showIncludes:user" {
			cmd.Args[i] = "/showIncludes"
		}
	}
}

func (depsMSVC) DepsAfterRun(ctx context.Context, b *Builder, step *Step) ([]string, error) {
	ctx, span := trace.NewSpan(ctx, "deps-for-msvc")
	defer span.Close(nil)
	if step.Cmd.Deps != "msvc" {
		return nil, fmt.Errorf("deps-for-msvc: unexpected deps=%q %s", step.Cmd.Deps, step)
	}
	// RBE doesn't use stderr?
	// http://b/149501385 stdout and stderr get merged in ActionResult
	output := step.Cmd.Stderr()
	output = append(output, step.Cmd.Stdout()...)
	_, dspan := trace.NewSpan(ctx, "parse-deps")
	deps, filteredOutput := msvcutil.ParseShowIncludes(output)
	dspan.SetAttr("deps", len(deps))
	dspan.Close(nil)
	clog.Infof(ctx, "deps-for-msvc stdout=%d stderr=%d -> deps=%d extra=%q", len(step.Cmd.Stdout()), len(step.Cmd.Stderr()), len(deps), filteredOutput)
	step.Cmd.StdoutWriter().Write(filteredOutput)
	step.Cmd.StderrWriter().Write(nil)
	// /showIncludes doesn't include source file.
	for _, arg := range step.Cmd.Args {
		switch ext := filepath.Ext(arg); ext {
		case ".cpp", ".cxx", ".cc", ".c", ".S", ".s":
			deps = append(deps, arg)
		}
	}
	return deps, nil
}

func (msvc depsMSVC) DepsCmd(ctx context.Context, b *Builder, step *Step) ([]string, error) {
	depsIns, err := msvc.depsInputs(ctx, b, step)
	msvc.fixArgsForDeps(step.Cmd)
	return depsIns, err
}

func (depsMSVC) depsInputs(ctx context.Context, b *Builder, step *Step) ([]string, error) {
	cwd := b.path.AbsFromWD(".")
	err := b.prepareLocalInputs(ctx, step)
	if err != nil {
		return nil, fmt.Errorf("prepare for msvc deps: %w", err)
	}
	dargs := step.Cmd.DepsArgs
	if len(dargs) == 0 {
		dargs = msvcutil.DepsArgs(step.Cmd.Args)
	}
	ins, err := msvcutil.Deps(ctx, dargs, nil, cwd)
	if err != nil {
		return nil, err
	}
	var inputs []string
	for _, in := range ins {
		inpath := b.path.MustFromWD(in)
		_, err := b.hashFS.Stat(ctx, b.path.ExecRoot, inpath)
		if err != nil {
			clog.Warningf(ctx, "missing inputs? %s: %v", inpath, err)
			continue
		}
		inputs = append(inputs, inpath)
	}
	return inputs, nil
}
