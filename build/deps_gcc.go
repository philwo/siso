// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"time"

	log "github.com/golang/glog"

	"infra/build/siso/execute"
	"infra/build/siso/o11y/clog"
	"infra/build/siso/o11y/trace"
	"infra/build/siso/reapi/merkletree"
	"infra/build/siso/scandeps"
	"infra/build/siso/toolsupport/gccutil"
	"infra/build/siso/toolsupport/makeutil"
)

type depsGCC struct {
	// for unittest
	treeInput func(context.Context, string) (merkletree.TreeEntry, error)
}

func (gcc depsGCC) DepsFastCmd(ctx context.Context, b *Builder, cmd *execute.Cmd) (*execute.Cmd, error) {
	newCmd := &execute.Cmd{}
	*newCmd = *cmd
	inputs, err := gcc.fixCmdInputs(ctx, b, newCmd)
	if err != nil {
		return nil, err
	}
	// sets include dirs + sysroots to ToolInputs.
	// Inputs will be overridden by deps log data.
	newCmd.ToolInputs = append(newCmd.ToolInputs, inputs...)
	gcc.fixForSplitDwarf(ctx, newCmd)
	return newCmd, nil
}

func (gcc depsGCC) fixCmdInputs(ctx context.Context, b *Builder, cmd *execute.Cmd) ([]string, error) {
	_, dirs, sysroots, _, err := gccutil.ScanDepsParams(ctx, cmd.Args, cmd.Env)
	if err != nil {
		return nil, err
	}
	for i := range dirs {
		dirs[i] = b.path.MustFromWD(dirs[i])
	}
	for i := range sysroots {
		sysroots[i] = b.path.MustFromWD(sysroots[i])
	}
	var inputs []string
	// include directory must be included, even if no include files there.
	// without the dir, it may fail for `#include "../config.h"`
	inputs = append(inputs, dirs...)
	// sysroot directory must be included, even if no include files there.
	// or error with
	// clang++: error: no such sysroot directory: ...
	// [-Werror, -Wmissing-sysroot]
	inputs = append(inputs, sysroots...)
	inputs = b.expandInputs(ctx, inputs)

	fixSymlink := func(name string, dirs []string) {
		for i := range dirs {
			fi, err := b.hashFS.Stat(ctx, b.path.ExecRoot, dirs[i])
			if err != nil {
				continue
			}
			if target := fi.Target(); target != "" {
				dir := dirs[i]
				dirs[i] = path.Join(path.Dir(dir), target)
				clog.Infof(ctx, "%s symlink: %s -> %s", name, dir, dirs[i])
			}
		}
	}
	fixSymlink("dirs", dirs)
	fixSymlink("sysroots", sysroots)

	fn := func(ctx context.Context, dir string) (merkletree.TreeEntry, error) {
		return b.treeInput(ctx, dir, ":headers", nil)
	}
	if gcc.treeInput != nil {
		fn = gcc.treeInput
	}
	cmd.TreeInputs = append(cmd.TreeInputs, treeInputs(ctx, fn, sysroots, dirs)...)
	return inputs, nil
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
	if err != nil {
		return nil, err
	}
	if step.def.Binding("use_remote_exec_wrapper") == "" && b.reapiclient != nil {
		// no need to upload precomputed subtree in inputs
		// when remote exec wrapper is used or not using reapi.
		// b/283867642
		inputs, err := gcc.fixCmdInputs(ctx, b, step.cmd)
		if err != nil {
			return nil, err
		}
		depsIns = append(depsIns, inputs...)
	}
	gcc.fixForSplitDwarf(ctx, step.cmd)
	return depsIns, err
}

func (gcc depsGCC) depsInputs(ctx context.Context, b *Builder, step *Step) ([]string, error) {
	ins, err := gcc.scandeps(ctx, b, step)
	if err == nil {
		return ins, nil
	}
	if errors.Is(err, context.Canceled) {
		return nil, err
	}
	step.metrics.ScandepsErr = true
	b.stats.scanDepsFailed(ctx, err)
	if !experiments.Enabled("scandeps-fallback", "scandeps failed, fallback to use `clang -M`") {
		return nil, err
	}
	// TODO(b/289142353): drop `clang -M` support?
	cwd := b.path.AbsFromWD(".")
	err = b.prepareLocalInputs(ctx, step)
	if err != nil {
		return nil, fmt.Errorf("prepare for gcc deps: %w", err)
	}
	dargs := step.cmd.DepsArgs
	if len(dargs) == 0 {
		dargs = gccutil.DepsArgs(step.cmd.Args)
	}
	ins, err = gccutil.Deps(ctx, dargs, nil, cwd)
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

func (depsGCC) scandeps(ctx context.Context, b *Builder, step *Step) ([]string, error) {
	var ins []string
	err := b.scanDepsSema.Do(ctx, func(ctx context.Context) error {
		var err error
		// need to use DepsArgs?
		files, dirs, sysroots, defines, err := gccutil.ScanDepsParams(ctx, step.cmd.Args, step.cmd.Env)
		if err != nil {
			return err
		}
		for i := range files {
			files[i] = b.path.MustFromWD(files[i])
		}
		for i := range dirs {
			dirs[i] = b.path.MustFromWD(dirs[i])
		}
		for i := range sysroots {
			sysroots[i] = b.path.MustFromWD(sysroots[i])
		}
		req := scandeps.Request{
			Defines:  defines,
			Sources:  files,
			Dirs:     dirs,
			Sysroots: sysroots,
		}
		if log.V(1) {
			clog.Infof(ctx, "scandeps req=%#v", req)
		}
		started := time.Now()
		ins, err = b.scanDeps.Scan(ctx, b.path.ExecRoot, req)
		if log.V(1) {
			clog.Infof(ctx, "scandeps %d %s: %v", len(ins), time.Since(started), err)
		}
		if err != nil {
			buf, berr := json.Marshal(req)
			clog.Warningf(ctx, "scandeps failed Request %s %v: %v", buf, berr, err)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	for i := range ins {
		ins[i] = b.path.Intern(ins[i])
	}
	return ins, nil
}
