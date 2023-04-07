// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	log "github.com/golang/glog"

	"infra/build/siso/execute"
	"infra/build/siso/o11y/clog"
	"infra/build/siso/o11y/trace"
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
	ds, found := depsProcessors[step.Cmd.Deps]
	if !found {
		return nil, fmt.Errorf("no fast-deps (deps=%q depfile=%q)", step.Cmd.Deps, step.Cmd.Depfile)
	}
	depsIns, err := step.Def.DepInputs(ctx)
	if err != nil {
		depsIns, err = fastDepsLogInputs(ctx, b, step.Cmd)
		if err != nil {
			return nil, fmt.Errorf("failed to get fast deps log (deps=%q): %v", step.Cmd.Deps, err)
		}
	}
	newCmd, err := ds.DepsFastCmd(ctx, b, step.Cmd)
	if err != nil {
		return nil, err
	}
	newCmd.ID += "-fast-deps"

	// Inputs may contains unnecessary inputs.
	// just needs ToolInputs.
	stepInputs := step.Cmd.ToolInputs
	if step.Cmd.Platform["OSFamily"] != "Windows" {
		depsIns = step.Def.ExpandCaseSensitives(ctx, depsIns)
	}
	inputs, err := fixInputsByDeps(ctx, b, stepInputs, depsIns)
	if err != nil {
		clog.Warningf(ctx, "failed to fix inputs by deps: %v", err)
		return nil, err
	}
	clog.Infof(ctx, "fix inputs by deps %d -> %d", len(step.Cmd.Inputs), len(inputs))
	newCmd.Inputs = inputs
	newCmd.Pure = true
	fastStep := &Step{}
	*fastStep = *step
	fastStep.Cmd = newCmd
	fastStep.FastDeps = true
	return fastStep, nil
}

func depsExpandInputs(ctx context.Context, b *Builder, step *Step) {
	ctx, span := trace.NewSpan(ctx, "deps-expand-inputs")
	defer span.Close(nil)
	// deps=gcc, msvc will get correct inputs from deps log,
	// so no need to expand inputs here.
	switch step.Cmd.Deps {
	case "gcc", "msvc":
	default:
		if step.Def.Binding("siso_handler") != "" {
			// already expanded before handle.
			return
		}
		oldlen := len(step.Cmd.Inputs)
		expanded := step.Def.ExpandedInputs(ctx)
		inputs := make([]string, 0, oldlen+len(expanded))
		seen := make(map[string]bool)
		for _, in := range step.Cmd.Inputs {
			if seen[in] {
				continue
			}
			seen[in] = true
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
		clog.Infof(ctx, "deps expands %d -> %d", len(step.Cmd.Inputs), len(inputs))
		step.Cmd.Inputs = make([]string, len(inputs))
		copy(step.Cmd.Inputs, inputs)
	}
}

func depsFixCmd(ctx context.Context, b *Builder, step *Step, deps []string) {
	stepInputs := step.Def.Inputs(ctx) // use ToolInputs?
	if step.Cmd.Platform["OSFamily"] != "Windows" {
		deps = step.Def.ExpandCaseSensitives(ctx, deps)
	}
	inputs, err := fixInputsByDeps(ctx, b, stepInputs, deps)
	if err != nil {
		clog.Warningf(ctx, "fix inputs by deps: %v", err)
		step.Cmd.Pure = false
		return
	}
	step.Cmd.Inputs = inputs
	step.Cmd.Pure = true
}

func depsCmd(ctx context.Context, b *Builder, step *Step) error {
	ds, found := depsProcessors[step.Cmd.Deps]
	if found {
		start := time.Now()
		// TODO: same as depsFastStep
		var stepInputs []string
		switch step.Cmd.Deps {
		case "gcc", "msvc":
			// Inputs may contains unnecessary inputs.
			// just needs ToolInputs for deps=gcc, msvc.
			stepInputs = step.Cmd.ToolInputs
		default:
			stepInputs = step.Def.Inputs(ctx) // use ToolInputs?
		}
		depsIns, err := ds.DepsCmd(ctx, b, step)
		if step.Cmd.Platform["OSFamily"] != "Windows" {
			depsIns = step.Def.ExpandCaseSensitives(ctx, depsIns)
		}
		inputs := uniqueFiles(stepInputs, depsIns)
		clog.Infof(ctx, "%s-deps %d %s: %v", step.Cmd.Deps, len(inputs), time.Since(start), err)
		if err != nil {
			return err
		}
		step.Cmd.Inputs = inputs
		// DepsCmd should set Pure=true or false.
	}
	return nil
}

func depsAfterRun(ctx context.Context, b *Builder, step *Step) ([]string, error) {
	ds, found := depsProcessors[step.Cmd.Deps]
	if !found {
		if log.V(1) {
			clog.Infof(ctx, "update deps; unexpected deps=%q", step.Cmd.Deps)
		}
		return nil, nil
	}
	deps, err := ds.DepsAfterRun(ctx, b, step)
	if err != nil {
		return nil, err
	}
	if step.Cmd.Platform["InputRootAbsolutePath"] == "" {
		for _, dep := range deps {
			if filepath.IsAbs(dep) {
				clog.Warningf(ctx, "update deps: abs path in deps %s in depfile=%s. use input_root_absolute_path. platform=%v", dep, step.Cmd.Depfile, step.Cmd.Platform)
				if step.Cmd.Pure {
					return nil, fmt.Errorf("absolute path in deps %s of %s: use input_root_absolute_path=true: %w", dep, step, errNotRelocatable)
				}
			}
		}
	}
	return deps, nil
}

// returns canonical paths
func fastDepsLogInputs(ctx context.Context, b *Builder, cmd *execute.Cmd) ([]string, error) {
	if len(cmd.Outputs) == 0 {
		return nil, fmt.Errorf("no output for %s", cmd)
	}
	out := b.path.MustToWD(cmd.Outputs[0])
	gctx, span := trace.NewSpan(ctx, "fast-deps-get")
	deps, _, err := b.sharedDepsLog.Get(gctx, out, cmd.CmdHash)
	span.Close(nil)
	if err != nil {
		return nil, fmt.Errorf("failed to lookups deps log for %s: %v", out, err)
	}
	for i := range deps {
		deps[i] = b.path.MustFromWD(deps[i])
	}
	clog.Infof(ctx, "fast-deps %q %d", out, len(deps))
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
