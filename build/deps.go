// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	log "github.com/golang/glog"

	"infra/build/siso/execute"
	"infra/build/siso/o11y/clog"
	"infra/build/siso/o11y/trace"
	"infra/build/siso/toolsupport/makeutil"
)

type depsProcessor interface {
	// create new cmd for fast deps run
	DepsFastCmd(context.Context, *Builder, *execute.Cmd) (*execute.Cmd, error)

	// fix step.cmd and returns deps inputs.
	// paths are execroot relative.
	DepsCmd(context.Context, *Builder, *Step) ([]string, error)

	// collects deps after cmd run.
	// paths are cwd relative.
	DepsAfterRun(context.Context, *Builder, *Step) ([]string, error)
}

var depsProcessors = map[string]depsProcessor{
	"depfile": depsDepfile{},
	"gcc":     depsGCC{},
	"msvc":    depsMSVC{},
}

func depsFastStep(ctx context.Context, b *Builder, step *Step) (*Step, error) {
	if b.reapiclient == nil {
		return nil, errors.New("no fast-deps (reapi is not configured)")
	}
	if step.useReclient() {
		return nil, fmt.Errorf("no fast-deps (use reclient)")
	}
	if len(step.cmd.Platform) == 0 || step.cmd.Platform["container-image"] == "" {
		return nil, errors.New("no fast-deps (no remote step)")
	}
	if experiments.Enabled("no-fast-deps", "disable fast-deps and force scandeps") {
		return nil, errors.New("no fast-deps (SISO_EXPERIMENTS)")
	}
	ds, found := depsProcessors[step.cmd.Deps]
	if !found {
		return nil, fmt.Errorf("no fast-deps (deps=%q depfile=%q)", step.cmd.Deps, step.cmd.Depfile)
	}
	var newCmd *execute.Cmd
	// protect from thread exhaustion,
	// because it would access many files, which would call syscalls
	// and may create new threads.
	err := b.scanDepsSema.Do(ctx, func(ctx context.Context) error {
		depsIns, err := step.def.DepInputs(ctx)
		if err != nil {
			return fmt.Errorf("failed to get fast deps log (deps=%q): %w", step.cmd.Deps, err)
		}
		newCmd, err = ds.DepsFastCmd(ctx, b, step.cmd)
		if err != nil {
			return err
		}
		newCmd.ID += "-fast-deps"
		// Inputs may contains unnecessary inputs.
		// just needs ToolInputs.
		stepInputs := newCmd.ToolInputs
		depsIns = step.def.ExpandedCaseSensitives(ctx, depsIns)
		inputs, err := fixInputsByDeps(ctx, b, stepInputs, depsIns)
		if err != nil {
			clog.Warningf(ctx, "failed to fix inputs by deps: %v", err)
			return err
		}
		clog.Infof(ctx, "fix inputs by deps %d -> %d", len(step.cmd.Inputs), len(inputs))
		newCmd.Inputs = inputs
		newCmd.Pure = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	fastStep := &Step{}
	*fastStep = *step
	fastStep.cmd = newCmd
	return fastStep, nil
}

// depsExpandInputs expands step.cmd.Inputs.
// result will not contain labels nor non-existing files.
func depsExpandInputs(ctx context.Context, b *Builder, step *Step) {
	ctx, span := trace.NewSpan(ctx, "deps-expand-inputs")
	defer span.Close(nil)
	// deps=gcc, msvc will get correct inputs from deps log,
	// so no need to expand inputs here.
	switch step.cmd.Deps {
	case "gcc", "msvc":
	default:
		if step.def.Binding("siso_handler") != "" {
			// already expanded before handle.
			return
		}
		oldlen := len(step.cmd.Inputs)
		expanded := step.def.ExpandedInputs(ctx)
		inputs := make([]string, 0, oldlen+len(expanded))
		seen := make(map[string]bool)
		for _, in := range step.cmd.Inputs {
			if seen[in] {
				continue
			}
			seen[in] = true
			// labels are expanded in expanded,
			// so no need to preserve it in inputs.
			if strings.Contains(in, ":") {
				if runtime.GOOS == "windows" && filepath.IsAbs(in) {
					if strings.Contains(in[2:], ":") {
						continue
					}
				} else {
					continue
				}
			}
			if _, err := b.hashFS.Stat(ctx, b.path.ExecRoot, in); err != nil {
				clog.Warningf(ctx, "deps stat error %s: %v", in, err)
				continue
			}
			inputs = append(inputs, in)
		}
		for _, in := range expanded {
			if seen[in] {
				continue
			}
			seen[in] = true
			if _, err := b.hashFS.Stat(ctx, b.path.ExecRoot, in); err != nil {
				clog.Warningf(ctx, "deps stat error %s: %v", in, err)
				continue
			}
			inputs = append(inputs, in)
		}
		clog.Infof(ctx, "deps expands %d -> %d", len(step.cmd.Inputs), len(inputs))
		step.cmd.Inputs = make([]string, len(inputs))
		copy(step.cmd.Inputs, inputs)
	}
}

func depsFixCmd(ctx context.Context, b *Builder, step *Step, deps []string) {
	stepInputs := step.def.Inputs(ctx) // use ToolInputs?
	deps = step.def.ExpandedCaseSensitives(ctx, deps)
	inputs, err := fixInputsByDeps(ctx, b, stepInputs, deps)
	if err != nil {
		clog.Warningf(ctx, "fix inputs by deps: %v", err)
		step.cmd.Pure = false
		return
	}
	step.cmd.Inputs = inputs
	step.cmd.Pure = true
}

func depsCmd(ctx context.Context, b *Builder, step *Step) error {
	started := time.Now()
	defer func() {
		step.metrics.DepsScanTime = IntervalMetric(time.Since(started))
	}()

	ds, found := depsProcessors[step.cmd.Deps]
	if found {
		start := time.Now()
		// TODO: same as depsFastStep
		var stepInputs []string
		switch step.cmd.Deps {
		case "gcc", "msvc":
			// Inputs may contains unnecessary inputs.
			// just needs ToolInputs for deps=gcc, msvc.
			stepInputs = step.cmd.ToolInputs
		default:
			stepInputs = step.def.Inputs(ctx) // use ToolInputs?
		}
		depsIns, err := ds.DepsCmd(ctx, b, step)
		depsIns = step.def.ExpandedCaseSensitives(ctx, depsIns)
		inputs := uniqueFiles(stepInputs, depsIns)
		clog.Infof(ctx, "%s-deps %d %s: %v", step.cmd.Deps, len(inputs), time.Since(start), err)
		if err != nil {
			return err
		}
		step.cmd.Inputs = inputs
		// DepsCmd should set Pure=true or false.
	}
	return nil
}

func depsAfterRun(ctx context.Context, b *Builder, step *Step) ([]string, error) {
	ds, found := depsProcessors[step.cmd.Deps]
	if !found {
		if log.V(1) {
			clog.Infof(ctx, "update deps; unexpected deps=%q", step.cmd.Deps)
		}
		// deps= is not set, but depfile= is set.
		if step.cmd.Depfile != "" {
			err := checkDepfile(ctx, b, step)
			if err != nil {
				return nil, err
			}
		}
		return nil, nil
	}
	deps, err := ds.DepsAfterRun(ctx, b, step)
	if err != nil {
		return nil, err
	}
	return deps, nil
}

func fixInputsByDeps(ctx context.Context, b *Builder, stepInputs, depsIns []string) ([]string, error) {
	ctx, span := trace.NewSpan(ctx, "fix-inputs-by-deps")
	defer span.Close(nil)
	span.SetAttr("deps-inputs", len(depsIns))
	entries, err := b.hashFS.Entries(ctx, b.path.ExecRoot, depsIns)
	if err != nil {
		return nil, fmt.Errorf("failed to get entries: %w", err)
	}
	inputs := stepInputs
	for _, ent := range entries {
		inputs = append(inputs, b.path.Intern(ent.Name))
		// no need to recursive expand?
		// log.Infof("%s fix-input %s => %s", step, ent.Name, ent.Target)
		if ent.Target != "" {
			inputs = append(inputs, b.path.Intern(filepath.Join(filepath.Dir(ent.Name), ent.Target)))
		}
	}
	inputs = uniqueFiles(inputs)
	return inputs, nil
}

func checkDepfile(ctx context.Context, b *Builder, step *Step) error {
	fsys := b.hashFS.FileSystem(ctx, b.path.ExecRoot)
	deps, err := makeutil.ParseDepsFile(ctx, fsys, step.cmd.Depfile)
	if err != nil {
		return fmt.Errorf("failed to parse depfile %s: %w", step.cmd.Depfile, err)
	}
	err = checkDeps(ctx, b, step, deps)
	if err != nil {
		return fmt.Errorf("error in depfile %s: %w", step.cmd.Depfile, err)
	}
	return nil
}

func checkDeps(ctx context.Context, b *Builder, step *Step, deps []string) error {
	// TODO: implement check that deps output (target in depfile "<target>: <dependencyList>") matches build graph's output.

	platform := step.cmd.Platform
	if step.useReclient() {
		platform = step.cmd.REProxyConfig.Platform
	}
	relocatableReq := platform["InputRootAbsolutePath"] == ""
	for _, dep := range deps {
		// remote relocatableReq should not have absolute path dep.
		if filepath.IsAbs(dep) {
			if relocatableReq {
				clog.Warningf(ctx, "check deps: abs path in deps %s: platform=%v", dep, platform)
				if platform != nil {
					return fmt.Errorf("absolute path in deps %s of %s: use input_root_absolute_path=true for %s (siso config: %s): %w", dep, step, step.cmd.Outputs[0], step.def.RuleName(), errNotRelocatable)
				}
			}
			continue
		}
		// all dep (== inputs) should exist just after step ran.
		input := b.path.MaybeFromWD(ctx, dep)
		_, err := b.hashFS.Stat(ctx, b.path.ExecRoot, input)
		if err != nil {
			return fmt.Errorf("deps input %s not exist: %w", dep, err)
		}
		// input should not be output.
		for _, out := range step.cmd.Outputs {
			if out == input {
				return fmt.Errorf("deps input %s is output", dep)
			}
		}
		// TODO: if dep is generated, check the dep is in ancestor inputs (direct or indirect inputs) to make sure no missing deps in build graph.
	}
	return nil
}
