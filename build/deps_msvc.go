// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	log "github.com/golang/glog"

	"infra/build/siso/execute"
	"infra/build/siso/o11y/clog"
	"infra/build/siso/o11y/trace"
	"infra/build/siso/reapi/merkletree"
	"infra/build/siso/scandeps"
	"infra/build/siso/toolsupport/msvcutil"
)

type depsMSVC struct {
	// for unittest
	treeInput func(context.Context, string) (merkletree.TreeEntry, error)
}

func (msvc depsMSVC) DepsFastCmd(ctx context.Context, b *Builder, cmd *execute.Cmd) (*execute.Cmd, error) {
	newCmd := &execute.Cmd{}
	*newCmd = *cmd
	inputs, err := msvc.fixCmdInputs(ctx, b, newCmd)
	if err != nil {
		return nil, err
	}
	// set include dirs + sysroots to ToolInputs
	// Inputs will be overridden by deps log data.
	newCmd.ToolInputs = append(newCmd.ToolInputs, inputs...)
	return newCmd, nil
}

func (msvc depsMSVC) fixCmdInputs(ctx context.Context, b *Builder, cmd *execute.Cmd) ([]string, error) {
	_, _, dirs, sysroots, _, err := msvcutil.ScanDepsParams(ctx, cmd.Args, cmd.Env)
	if err != nil {
		return nil, err
	}
	for i := range dirs {
		dirs[i] = b.path.MustFromWD(dirs[i])
	}
	for i := range sysroots {
		sysroots[i] = b.path.MustFromWD(sysroots[i])
	}
	clog.Infof(ctx, "fixCmdInputs dirs=%q sysroots=%q", dirs, sysroots)
	var inputs []string
	// include directory must be included, even if no include files there.
	// without the dir, it may fail for `#include "../config.h"`
	inputs = append(inputs, dirs...)
	// sysroot directory must be included, eve if no include files there.
	// or error with
	// clang++: error: no such sysroot directory: ... [-Werror, -Wmissing-sysroot]
	inputs = append(inputs, sysroots...)
	inputs = b.expandInputs(ctx, inputs)

	var expandFn func(context.Context, []string) []string
	if cmd.Platform["OSFamily"] != "Windows" {
		clog.Infof(ctx, "expand case sensitive includes")
		expandFn = func(ctx context.Context, files []string) []string {
			return expandCPPCaseSensitiveIncludes(ctx, b, files)
		}
	} else {
		clog.Infof(ctx, "cmd platform=%q", cmd.Platform)
	}

	fn := func(ctx context.Context, dir string) (merkletree.TreeEntry, error) {
		return b.treeInput(ctx, dir, ":headers", expandFn)
	}
	if msvc.treeInput != nil {
		fn = msvc.treeInput
	}
	cmd.TreeInputs = append(cmd.TreeInputs, treeInputs(ctx, fn, sysroots, dirs)...)
	clog.Infof(ctx, "treeInputs=%v", cmd.TreeInputs)
	return inputs, nil
}

func (depsMSVC) DepsAfterRun(ctx context.Context, b *Builder, step *Step) ([]string, error) {
	ctx, span := trace.NewSpan(ctx, "deps-for-msvc")
	defer span.Close(nil)
	if step.cmd.Deps != "msvc" {
		return nil, fmt.Errorf("deps-for-msvc: unexpected deps=%q %s", step.cmd.Deps, step)
	}
	// RBE doesn't use stderr?
	// http://b/149501385 stdout and stderr get merged in ActionResult
	output := step.cmd.Stderr()
	output = append(output, step.cmd.Stdout()...)
	_, dspan := trace.NewSpan(ctx, "parse-deps")
	deps, filteredOutput := msvcutil.ParseShowIncludes(output)

	m := make(map[string]bool)
	basenames := make(map[string]bool)
	for _, d := range deps {
		m[d] = true
		basenames[filepath.Base(d)] = true
	}
	// clang-cl /showIncludes doesn't report correctly.
	// b/294927170 https://github.com/llvm/llvm-project/issues/58726
	// so records cmd.Inputs used for the same basename.
	// cmd.Inputs may include unnecessary inputs that may break
	// confirm no-op b/307834469
	for _, in := range step.cmd.Inputs {
		if !basenames[filepath.Base(in)] {
			// include basename matched files only.
			// other files/dirs may add unnecessary deps
			// and break no-op check.
			continue
		}
		in = b.path.MustToWD(in)
		if m[in] {
			continue
		}
		m[in] = true
		deps = append(deps, in)
	}

	dspan.SetAttr("deps", len(deps))
	dspan.Close(nil)

	step.cmd.StdoutWriter().Write(filteredOutput)
	step.cmd.StderrWriter().Write(nil)
	// /showIncludes doesn't include source file.
	for _, arg := range step.cmd.Args {
		switch ext := filepath.Ext(arg); ext {
		case ".cpp", ".cxx", ".cc", ".c", ".S", ".s":
			if m[arg] {
				continue
			}
			m[arg] = true
			deps = append(deps, arg)
		}
	}
	clog.Infof(ctx, "deps-for-msvc stdout=%d stderr=%d -> deps=%d inputs=%d extra=%q", len(step.cmd.Stdout()), len(step.cmd.Stderr()), len(deps), len(step.cmd.Inputs), filteredOutput)
	return deps, nil
}

func (msvc depsMSVC) DepsCmd(ctx context.Context, b *Builder, step *Step) ([]string, error) {
	depsIns, err := msvc.depsInputs(ctx, b, step)
	if err != nil {
		return nil, err
	}
	if step.def.Binding("use_remote_exec_wrapper") == "" && b.reapiclient != nil {
		// no need to upload precomputed subtree in inputs
		// when remote exec wrapper is used or reapi is not used.
		// b/283867642
		inputs, err := msvc.fixCmdInputs(ctx, b, step.cmd)
		if err != nil {
			return nil, err
		}
		depsIns = append(depsIns, inputs...)
	}
	return depsIns, err
}

func (msvc depsMSVC) depsInputs(ctx context.Context, b *Builder, step *Step) ([]string, error) {
	ins, err := msvc.scandeps(ctx, b, step)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			step.metrics.ScandepsErr = true
		}
		return nil, err
	}
	return ins, nil
}

func (depsMSVC) scandeps(ctx context.Context, b *Builder, step *Step) ([]string, error) {
	var ins []string
	err := b.scanDepsSema.Do(ctx, func(ctx context.Context) error {
		files, includes, dirs, sysroots, defines, err := msvcutil.ScanDepsParams(ctx, step.cmd.Args, step.cmd.Env)
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
			Includes: includes,
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

func expandCPPCaseSensitiveIncludes(ctx context.Context, b *Builder, files []string) []string {
	clog.Infof(ctx, "expand cs %d", len(files))
	seen := make(map[string]bool)
	includeNames := make(map[string]bool)
	includePaths := make(map[string][]string)
	for _, f := range files {
		if seen[f] {
			continue
		}
		seen[f] = true
		switch strings.ToLower(filepath.Ext(f)) {
		case ".h", ".hxx", ".hpp", ".inc":
			clog.Infof(ctx, "expand cs %s -> header", f)
		default:
			clog.Infof(ctx, "expand cs %s -> ignore", f)
			continue
		}

		inc := strings.ToLower(filepath.Base(f))
		includePaths[inc] = append(includePaths[inc], filepath.ToSlash(filepath.Dir(f)))
		inc = strings.ToLower(filepath.ToSlash(filepath.Join(filepath.Base(filepath.Dir(f)), filepath.Base(f))))
		includePaths[inc] = append(includePaths[inc], filepath.ToSlash(filepath.Dir(filepath.Dir(f))))

		buf, err := b.hashFS.ReadFile(ctx, b.path.ExecRoot, f)
		if err != nil {
			clog.Warningf(ctx, "expand cs: failed to read %s: %v", f, err)
			continue
		}
		includes, _, err := scandeps.CPPScan(ctx, f, buf)
		if err != nil {
			clog.Warningf(ctx, "expand cs: failed to scan %s: %v", f, err)
			continue
		}
		for _, inc := range includes {
			switch {
			case strings.HasPrefix(inc, `"`) && strings.HasSuffix(inc, `"`):
				inc = inc[1 : len(inc)-1]
			case strings.HasPrefix(inc, "<") && strings.HasSuffix(inc, ">"):
				inc = inc[1 : len(inc)-1]
			default:
				// macro include?
				continue
			}
			includeNames[inc] = true
		}
	}
	for inc := range includeNames {
		clog.Infof(ctx, "expand cs for inc:%s", inc)
		switch strings.Count(inc, "/") {
		case 0, 1:
			for _, dir := range includePaths[strings.ToLower(inc)] {
				f := filepath.ToSlash(filepath.Join(dir, inc))
				if seen[f] {
					continue
				}
				seen[f] = true
				clog.Infof(ctx, "expand cs %s -> %s", inc, f)
				files = append(files, f)
			}
		default:
			for _, f := range files {
				if strings.HasSuffix(strings.ToLower(f), "/"+inc) {
					f = f[:len(f)-len(inc)]
					f = filepath.ToSlash(filepath.Join(f, inc))
					if seen[f] {
						continue
					}
					seen[f] = true
					clog.Infof(ctx, "expand cs %s -> %s", inc, f)
					files = append(files, f)
				}
			}
		}
	}
	sort.Strings(files)
	return files
}
