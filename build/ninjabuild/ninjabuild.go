// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package ninjabuild provides build steps by ninja.
package ninjabuild

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"infra/build/siso/build"
	"infra/build/siso/build/buildconfig"
	"infra/build/siso/hashfs"
	"infra/build/siso/o11y/clog"
	"infra/build/siso/toolsupport/ninjautil"
)

// Graph holds build graph, i.e. all step defs described in build.ninja.
type Graph struct {
	fname  string
	nstate *ninjautil.State

	visited map[*ninjautil.Edge]bool

	globals *globals
}

type globals struct {
	path    *build.Path
	hashFS  *hashfs.HashFS
	depsLog *ninjautil.DepsLog

	buildConfig *buildconfig.Config
	stepConfig  *StepConfig

	// *ninjautil.Node -> string
	targetPaths sync.Map

	// output -> *edgeRule that produces the output
	edgeRules sync.Map

	phony map[string]bool

	// caseSensitives lists all case sensitive input filenames.
	caseSensitives map[string][]string

	// edge will be associated with the gn target.
	gnTargets map[*ninjautil.Edge]gnTarget
}

type gnTarget struct {
	target string
	rule   string
	pref   int
}

var gnTargetsRulePrefs = []string{"phony", "stamp", "solink", "alink", "link"}

// rulePref returns preference for gn targets of the rule.
// 0 is most preferred. the bigger, the less preferred.
// rule may have target arch prefix, so just use the last term separated by _.
// e.g. "clang_newlib_x64_solink" -> pref of "solink".
func rulePref(rule string) int {
	r := strings.Split(rule, "_")
	rule = r[len(r)-1]
	for i, p := range gnTargetsRulePrefs {
		if p == rule {
			return i
		}
	}
	return len(gnTargetsRulePrefs)
}

func (g gnTarget) String() string {
	return g.target
}

// NewStepConfig creates new *StepConfig and stores it in .siso_config
// and .siso_filegroups.
func NewStepConfig(ctx context.Context, config *buildconfig.Config, p *build.Path, hashFS *hashfs.HashFS, fname string) (*StepConfig, error) {
	s, err := config.Init(ctx, hashFS, p)
	if err != nil {
		return nil, err
	}
	stepConfig := &StepConfig{}
	err = json.Unmarshal([]byte(s), stepConfig)
	if err != nil {
		clog.Errorf(ctx, "Failed to parse init output:\n%s", s)
		return nil, fmt.Errorf("failed to parse init output: %w", err)
	}
	clog.Infof(ctx, "loaded %d platforms / %d input deps / %d rules", len(stepConfig.Platforms), len(stepConfig.InputDeps), len(stepConfig.Rules))
	buf, err := json.MarshalIndent(stepConfig, "", " ")
	if err != nil {
		return nil, fmt.Errorf("failed to marshal config: %v", err)
	}
	err = os.WriteFile(".siso_config", buf, 0644)
	if err != nil {
		return nil, err
	}
	clog.Infof(ctx, "save to .siso_config")
	err = stepConfig.Init(ctx)
	if err != nil {
		return nil, err
	}
	err = updateFilegroups(ctx, config, p, hashFS, fname, stepConfig)
	if err != nil {
		return nil, err
	}
	return stepConfig, nil
}

// updateFilegroups updates filegroups in *StepConfig and stores it in .siso_filegroups.
func updateFilegroups(ctx context.Context, config *buildconfig.Config, buildPath *build.Path, hashFS *hashfs.HashFS, fname string, sc *StepConfig) error {
	var fnameTime, fgTime time.Time
	fi, err := os.Stat(fname)
	if err != nil {
		return err
	}
	fnameTime = fi.ModTime()
	fi, err = os.Stat(".siso_filegroups")
	if err == nil {
		fgTime = fi.ModTime()
	}
	var fg buildconfig.Filegroups
	// initializes fg with valid cached result.
	if fnameTime.Before(fgTime) {
		buf, err := os.ReadFile(".siso_filegroups")
		if err != nil {
			clog.Warningf(ctx, "Failed to load .siso_filegroups: %v", err)
		} else {
			err = json.Unmarshal(buf, &fg)
			if err != nil {
				clog.Warningf(ctx, "Failed to unmarshal filegroups: %v", err)
			} else {
				clog.Infof(ctx, "loaded %d filegroups", len(fg.Filegroups))
			}
		}
	}
	started := time.Now()
	defer func() {
		clog.Infof(ctx, "update filegroups in %s", time.Since(started))
	}()
	fg, err = config.UpdateFilegroups(ctx, hashFS, buildPath, fg)
	if err != nil {
		return err
	}
	clog.Infof(ctx, "updated %d filegroups", len(fg.Filegroups))
	buf, err := json.MarshalIndent(fg, "", " ")
	if err != nil {
		return fmt.Errorf("failed to marshal filegroups: %v", err)
	}
	err = os.WriteFile(".siso_filegroups", buf, 0644)
	if err != nil {
		return err
	}
	clog.Infof(ctx, "save to .siso_filegroups")
	return sc.UpdateFilegroups(ctx, fg.Filegroups)
}

// Load loads build.ninja file specified by fname and returns parsed states.
func Load(ctx context.Context, fname string, buildPath *build.Path) (*ninjautil.State, error) {
	started := time.Now()
	state := ninjautil.NewState()
	state.AddBinding("exec_root", buildPath.ExecRoot)
	state.AddBinding("working_directory", buildPath.Dir)
	p := ninjautil.NewManifestParser(state)
	err := p.Load(ctx, fname)
	if err != nil {
		return nil, fmt.Errorf("failed to load %s: %w", fname, err)
	}
	clog.Infof(ctx, "load %s %s", fname, time.Since(started))
	return state, nil
}

// NewGraph creates new Graph from fname (usually "build.ninja") with stepConfig.
func NewGraph(ctx context.Context, fname string, nstate *ninjautil.State, config *buildconfig.Config, p *build.Path, hashFS *hashfs.HashFS, stepConfig *StepConfig, depsLog *ninjautil.DepsLog) *Graph {
	graph := &Graph{
		fname:  fname,
		nstate: nstate,

		visited: make(map[*ninjautil.Edge]bool),

		globals: &globals{
			path:           p,
			hashFS:         hashFS,
			depsLog:        depsLog,
			buildConfig:    config,
			stepConfig:     stepConfig,
			phony:          make(map[string]bool),
			caseSensitives: make(map[string][]string),
			gnTargets:      make(map[*ninjautil.Edge]gnTarget),
		},
	}
	graph.initGlobals(ctx)
	return graph
}

// Reload reloads hashfs, filegroups and build.ninja.
func (g *Graph) Reload(ctx context.Context) error {
	eg, ctx := errgroup.WithContext(ctx)
	eg.Go(func() error {
		// need to refresh cached entries as `gn gen` updated files
		// but ninja manifest doesn't know what files are updated.
		err := g.globals.hashFS.Refresh(ctx, g.globals.path.ExecRoot)
		if err != nil {
			return err
		}
		g.globals.stepConfig, err = NewStepConfig(ctx, g.globals.buildConfig, g.globals.path, g.globals.hashFS, g.fname)
		return err
	})
	eg.Go(func() error {
		var err error
		g.nstate, err = Load(ctx, g.fname, g.globals.path)
		return err
	})
	err := eg.Wait()
	if err != nil {
		return err
	}
	g.Reset(ctx)
	return nil
}

// Reset resets graph status.
func (g *Graph) Reset(ctx context.Context) {
	g.visited = make(map[*ninjautil.Edge]bool)
	g.globals.phony = make(map[string]bool)
	g.globals.caseSensitives = make(map[string][]string)
	g.globals.gnTargets = make(map[*ninjautil.Edge]gnTarget)
	g.initGlobals(ctx)
}

func (g *Graph) initGlobals(ctx context.Context) {
	// initialize caseSensitives.
	for _, f := range g.globals.stepConfig.CaseSensitiveInputs {
		cif := strings.ToLower(f)
		g.globals.caseSensitives[cif] = append(g.globals.caseSensitives[cif], f)
	}

	// infer gn target.
	// gn target will be node that is phony target and contains ":"
	// in its name.
	// e.g.
	//
	//   build base$:base: phony obj/base/libbase.a
	//
	//   build obj/base/libbase.a: alink obj/base/base/allocator_check.o ...
	//   build obj/base/base/allocator_check.o: cxx ../../base/allocator/allocator_check.cc
	//
	// A edge generating the `target` (e.g. alink for `obj/base/libbase.a`)
	// will have gn target `base:base`.
	// Also explicit inputs of the edge also have the gn target.
	// e.g. cxx for `obj/base/base/allocator_check.o` have `base:base`.
	//
	// If there are several paths from gn targets, choose based on
	// rule's preference.
	//
	// e.g.
	//   build v8/tools/wasm$:wami: phony ./wami
	//
	//   build ./wami: link obj/v8/tools/wasm/wami/module-inspector.o ..
	//       ... obj/v8/v8_compiler/access-builder.o ..
	//
	//   build v8$:v8_compiler: phony obj/v8/v8_compiler.stamp
	//   build obj/v8/v8_compiler.stamp: stamp obj/v8/v8_compiler/access-builder.o o ..
	//
	// link for ./wami:        gn target v8/tools/wasm:wami
	// cxx for obj/v8/tools/wasm/wami/module-inspector.o:
	//                         gn target v8/tools/wasm:wami
	// stamp for obj/v8/v8_compiler.stamp:
	//                         gn target v8:v8_compiler
	// cxx for obj/v8/v8_compiler/access-builder.o:
	//                         gn target v8:v8_compiler
	//                         (prefer stamp over link)
	started := time.Now()
	nGNTargets := 0
	for _, p := range g.nstate.PhonyNodes() {
		target := p.Path()
		if !strings.Contains(target, ":") {
			continue
		}
		e, ok := p.InEdge()
		if !ok {
			continue
		}
		nGNTargets++
		// gnTarget's phony inputs will use the gn target.
		for _, n := range e.Ins() {
			ne, ok := n.InEdge()
			if !ok {
				continue
			}
			g.globals.gnTargets[ne] = gnTarget{
				target: target,
				rule:   "phony",
				pref:   0,
			}

			rule := ne.Rule().Name()
			pref := rulePref(rule)
			for _, cn := range ne.Ins() {
				ce, ok := cn.InEdge()
				if !ok {
					continue
				}
				gt, ok := g.globals.gnTargets[ce]
				if ok && gt.pref <= pref {
					continue
				}
				g.globals.gnTargets[ce] = gnTarget{
					target: target,
					rule:   rule,
					pref:   pref,
				}
			}
		}
	}
	clog.Infof(ctx, "gn_targets=%d edges=%d in %s", nGNTargets, len(g.globals.gnTargets), time.Since(started))
}

// Filename returns filename of build manifest (e.g. build.ninja).
func (g *Graph) Filename() string {
	return g.fname
}

// Filenames returns filenames of build manifest (all files loaded by build.ninja).
func (g *Graph) Filenames() []string {
	return g.nstate.Filenames()
}

// Targets returns targets for ninja args.
// If args is not given, returns default build targets.
func (g *Graph) Targets(ctx context.Context, args ...string) ([]build.Target, error) {
	if len(args) == 0 {
		nodes, err := g.nstate.DefaultNodes()
		if err != nil {
			return nil, err
		}
		targets := make([]build.Target, 0, len(nodes))
		for _, n := range nodes {
			targets = append(targets, build.Target(n))
		}
		return targets, nil
	}
	targets := make([]build.Target, 0, len(args))
	for _, t := range args {
		t := filepath.ToSlash(t)
		var node *ninjautil.Node
		if strings.HasSuffix(t, "^") {
			// Special syntax: "foo.cc^" means "the first output of foo.cc".
			t = strings.TrimSuffix(t, "^")
			n, ok := g.nstate.LookupNode(t)
			if !ok {
				return nil, fmt.Errorf("unknown target %q", t)
			}
			outs := n.OutEdges()
			if len(outs) == 0 {
				// TODO(b/289309062): deps log first reverse deps node?
				return nil, fmt.Errorf("no outs for %q", t)
			}
			edge := outs[0]
			outputs := edge.Outputs()
			if len(outputs) == 0 {
				return nil, fmt.Errorf("out edge of %q has no output", t)
			}
			node = outputs[0]
		} else {
			n, ok := g.nstate.LookupNode(t)
			if !ok {
				return nil, fmt.Errorf("unknown target %q", t)
			}
			node = n
		}
		targets = append(targets, build.Target(node))
	}
	return targets, nil
}

// TargetPath returns exec-root relative path of the target.
func (g *Graph) TargetPath(target build.Target) (string, error) {
	node, ok := target.(*ninjautil.Node)
	if !ok {
		return "", fmt.Errorf("unexpected target type %T", target)
	}
	return g.globals.targetPath(node), nil
}

func (g *globals) targetPath(node *ninjautil.Node) string {
	p, ok := g.targetPaths.Load(node)
	if ok {
		return p.(string)
	}
	s := g.path.MaybeFromWD(node.Path())
	p, _ = g.targetPaths.LoadOrStore(node, s)
	return p.(string)
}

// StepDef creates new StepDef to build target (exec-root relative), needed for next.
// top-level target will use nil for next.
// It returns a StepDef for the target and inputs/outputs targets.
func (g *Graph) StepDef(ctx context.Context, target build.Target, next build.StepDef) (build.StepDef, []build.Target, []build.Target, error) {
	n, ok := target.(*ninjautil.Node)
	if !ok {
		return nil, nil, nil, build.ErrNoTarget
	}
	edge, ok := n.InEdge()
	if !ok {
		return nil, nil, nil, build.ErrTargetIsSource
	}
	if g.visited[edge] {
		return nil, nil, nil, build.ErrDuplicateStep
	}
	g.visited[edge] = true
	if edge.IsPhony() {
		g.globals.phony[g.globals.targetPath(n)] = true
	}
	stepDef := g.newStepDef(ctx, edge, next)
	edgeInputs := edge.Inputs()
	inputs := make([]build.Target, 0, len(edgeInputs))
	for _, in := range edgeInputs {
		inputs = append(inputs, build.Target(in))
	}
	edgeOutputs := edge.Outputs()
	outputs := make([]build.Target, 0, len(edgeOutputs))
	for _, out := range edgeOutputs {
		outputs = append(outputs, build.Target(out))
	}
	return stepDef, inputs, outputs, nil
}

// InputDeps returns input deps.
func (g *Graph) InputDeps(ctx context.Context) map[string][]string {
	return g.globals.stepConfig.InputDeps
}

// StepLimits returns a map of maximum number of concurrent steps by pool name.
func (g *Graph) StepLimits(ctx context.Context) map[string]int {
	m := make(map[string]int)
	for k, v := range g.nstate.Pools() {
		if v == nil || v.Depth() == 0 {
			continue
		}
		m[k] = v.Depth()
	}
	return m
}

// CleanDead cleans dead generated files, and
// returns number of removed files and number of last generated files.
func (g *Graph) CleanDead(ctx context.Context) (int, int, error) {
	started := time.Now()
	var deads []string
	dir := filepath.Join(g.globals.path.ExecRoot, g.globals.path.Dir)
	genFiles := g.globals.hashFS.PreviouslyGeneratedFiles()
	for _, genFile := range genFiles {
		rel, err := filepath.Rel(dir, genFile)
		if err != nil {
			return len(deads), len(genFiles), err
		}
		rel = filepath.ToSlash(rel)
		if g.isDead(rel) {
			deads = append(deads, rel)
			err := g.globals.hashFS.Remove(ctx, dir, rel)
			if err != nil {
				return len(deads), len(genFiles), err
			}
			clog.Infof(ctx, "deadfile %s", rel)
		}
	}
	var err error
	if len(deads) > 0 {
		err = g.globals.hashFS.Flush(ctx, dir, deads)
	}
	if err != nil {
		clog.Warningf(ctx, "cleandead %d/%d %s: %v", len(deads), len(genFiles), time.Since(started), err)

	} else {
		clog.Infof(ctx, "cleandead %d/%d %s", len(deads), len(genFiles), time.Since(started))
	}
	return len(deads), len(genFiles), err
}

// isDead reports fname is dead generated file or not.
// i.e. it is considered as dead if one of the following conditions is met.
//   - it is not used in current ninja build graph.
//   - if it is not generated by any step and not used by any steps.
//
// https://github.com/ninja-build/ninja/blob/a524bf3f6bacd1b4ad85d719eed2737d8562f27a/src/clean.cc#L141
func (g *Graph) isDead(fname string) bool {
	n, ok := g.nstate.LookupNode(fname)
	if !ok {
		return true
	}
	_, ok = n.InEdge()
	outs := n.OutEdges()
	return !ok && len(outs) == 0
}
