// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
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
	ds, found := depsProcessors[step.cmd.Deps]
	if !found {
		return nil, fmt.Errorf("no fast-deps (deps=%q depfile=%q)", step.cmd.Deps, step.cmd.Depfile)
	}
	depsIns, err := step.def.DepInputs(ctx)
	if err != nil {
		depsIns, err = fastDepsLogInputs(ctx, b, step.cmd)
		if err != nil {
			return nil, fmt.Errorf("failed to get fast deps log (deps=%q): %v", step.cmd.Deps, err)
		}
	}
	newCmd, err := ds.DepsFastCmd(ctx, b, step.cmd)
	if err != nil {
		return nil, err
	}
	newCmd.ID += "-fast-deps"

	// Inputs may contains unnecessary inputs.
	// just needs ToolInputs.
	stepInputs := step.cmd.ToolInputs
	if step.cmd.Platform["OSFamily"] != "Windows" {
		depsIns = step.def.ExpandCaseSensitives(ctx, depsIns)
	}
	inputs, err := fixInputsByDeps(ctx, b, stepInputs, depsIns)
	if err != nil {
		clog.Warningf(ctx, "failed to fix inputs by deps: %v", err)
		return nil, err
	}
	clog.Infof(ctx, "fix inputs by deps %d -> %d", len(step.cmd.Inputs), len(inputs))
	newCmd.Inputs = inputs
	newCmd.Pure = true
	fastStep := &Step{}
	*fastStep = *step
	fastStep.cmd = newCmd
	fastStep.fastDeps = true
	return fastStep, nil
}

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
	if step.cmd.Platform["OSFamily"] != "Windows" {
		deps = step.def.ExpandCaseSensitives(ctx, deps)
	}
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
	// TODO(b/273407069): native integration instead of spwaning gomacc/rewrapper?
	if strings.Contains(step.cmd.Args[0], "gomacc") || strings.Contains(step.cmd.Args[0], "rewrapper") {
		// no need to run `clang -M`.
		return nil
	}
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
		if step.cmd.Platform["OSFamily"] != "Windows" {
			depsIns = step.def.ExpandCaseSensitives(ctx, depsIns)
		}
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
		return nil, nil
	}
	deps, err := ds.DepsAfterRun(ctx, b, step)
	if err != nil {
		return nil, err
	}
	if step.cmd.Platform["InputRootAbsolutePath"] == "" {
		for _, dep := range deps {
			if filepath.IsAbs(dep) {
				clog.Warningf(ctx, "update deps: abs path in deps %s in depfile=%s. use input_root_absolute_path. platform=%v", dep, step.cmd.Depfile, step.cmd.Platform)
				if step.cmd.Pure {
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
