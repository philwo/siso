// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	log "github.com/golang/glog"

	"infra/build/siso/execute"
	"infra/build/siso/o11y/clog"
	"infra/build/siso/o11y/trace"
	"infra/build/siso/toolsupport/gccutil"
	"infra/build/siso/toolsupport/makeutil"
)

type depsGCC struct{}

func (gcc depsGCC) DepsFastCmd(ctx context.Context, b *Builder, cmd *execute.Cmd) (*execute.Cmd, error) {
	newCmd := &execute.Cmd{}
	*newCmd = *cmd
	newCmd.Inputs = gcc.sysroot(ctx, b, newCmd)
	gcc.fixArgsForDeps(ctx, newCmd)
	gcc.fixForSplitDwarf(ctx, newCmd)
	return newCmd, nil
}

// TODO: use handler?
func (depsGCC) sysroot(ctx context.Context, b *Builder, cmd *execute.Cmd) []string {
	// sysroot directory must be included, even if no include files there.
	// or error with
	// clang++: error: no such sysroot directory: ...
	// [-Werror, -Wmissing-sysroot]
	for i, arg := range cmd.Args[:len(cmd.Args)-1] {
		switch arg {
		case "--sysroot", "-isysroot":
			sysroot := cmd.Args[i+1]
			if log.V(1) {
				clog.Infof(ctx, "sysroot=%q", sysroot)
			}
			return []string{b.path.MustFromWD(sysroot)}
		}
	}
	if log.V(1) {
		clog.Infof(ctx, "no sysroot")
	}
	return nil
}

func (depsGCC) fixArgsForDeps(ctx context.Context, cmd *execute.Cmd) {
	// siso fast deps requires full dependency info,
	// but -MMD only generates user dependency.
	for i, arg := range cmd.Args {
		if arg == "-MMD" {
			cmd.Args[i] = "-MD"
		}
	}
}

// TODO: use handler?
func (depsGCC) fixForSplitDwarf(ctx context.Context, cmd *execute.Cmd) {
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
	clog.Infof(ctx, "add %s", dwo)
	cmd.Outputs = uniqueFiles(cmd.Outputs, []string{dwo})
}

func (depsGCC) DepsAfterRun(ctx context.Context, b *Builder, step *Step) ([]string, error) {
	ctx, span := trace.NewSpan(ctx, "gcc-deps")
	defer span.Close(nil)
	if step.cmd.Deps != "gcc" {
		return nil, fmt.Errorf("gcc-deps; unexpected deps=%q %s", step.cmd.Deps, step)
	}
	defer func() {
		b.hashFS.Remove(ctx, step.cmd.ExecRoot, step.cmd.Depfile)
		b.hashFS.Flush(ctx, step.cmd.ExecRoot, []string{step.cmd.Depfile})
	}()
	buf, err := b.hashFS.ReadFile(ctx, step.cmd.ExecRoot, step.cmd.Depfile)
	if err != nil {
		return nil, fmt.Errorf("gcc-deps: failed to get depfile %s of %s: %v", step.cmd.Depfile, step, err)
	}
	span.SetAttr("depfile", step.cmd.Depfile)
	span.SetAttr("deps-file-size", len(buf))

	_, dspan := trace.NewSpan(ctx, "parse-deps")
	deps := makeutil.ParseDeps(buf)
	dspan.SetAttr("deps", len(deps))
	dspan.Close(nil)
	return deps, nil
}

func (gcc depsGCC) DepsCmd(ctx context.Context, b *Builder, step *Step) ([]string, error) {
	depsIns, err := gcc.depsInputs(ctx, b, step)
	depsIns = append(depsIns, gcc.sysroot(ctx, b, step.cmd)...)
	gcc.fixArgsForDeps(ctx, step.cmd)
	gcc.fixForSplitDwarf(ctx, step.cmd)
	return depsIns, err
}

func (depsGCC) depsInputs(ctx context.Context, b *Builder, step *Step) ([]string, error) {
	cwd := b.path.AbsFromWD(".")
	err := b.prepareLocalInputs(ctx, step)
	if err != nil {
		return nil, fmt.Errorf("prepare for gcc deps: %w", err)
	}
	dargs := step.cmd.DepsArgs
	if len(dargs) == 0 {
		dargs = gccutil.DepsArgs(step.cmd.Args)
	}
	ins, err := gccutil.Deps(ctx, dargs, nil, cwd)
	if err != nil {
		return nil, err
	}
	var inputs []string
	for _, in := range ins {
		// TODO: need to preserve intermediate dirs
		// e.g.
		//  /usr/local/google/home/ukai/src/chromium/src/native_client/toolchain/linux_x86/nacl_x86_glibc/bin/../lib/gcc/x86_64-nacl/4.4.3/../../../../x86_64-nacl/include/stdint.h
		inpath := b.path.MustFromWD(in)
		fi, err := b.hashFS.Stat(ctx, b.path.ExecRoot, inpath)
		if err != nil {
			clog.Warningf(ctx, "missing inputs? %s: %v", inpath, err)
			continue
		}
		inputs = append(inputs, inpath)
		if target := fi.Target(); target != "" {
			inputs = append(inputs, b.path.MustFromWD(filepath.Join(filepath.Dir(in), target)))
		}
	}
	return inputs, nil
}
