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
	"strings"
	"sync"
	"time"

	"infra/build/siso/build"
	"infra/build/siso/build/buildconfig"
	"infra/build/siso/hashfs"
	"infra/build/siso/o11y/clog"
	"infra/build/siso/toolsupport/ninjautil"
	"infra/build/siso/ui"
)

// Graph holds build graph, i.e. all step defs described in build.ninja.
type Graph struct {
	fname   string
	once    sync.Once
	initErr error
	nstate  *ninjautil.State

	visited map[*ninjautil.Edge]bool

	globals *globals
}

type globals struct {
	path    *build.Path
	hashFS  *hashfs.HashFS
	depsLog *ninjautil.DepsLog

	buildConfig    *buildconfig.Config
	stepConfig     *StepConfig
	replaces       map[string][]string
	accumulates    map[string][]string
	caseSensitives map[string][]string
	phony          map[string]bool
}

// NewGraph creates new Graph from fname (usually "build.ninja") with stepConfig.
func NewGraph(ctx context.Context, fname string, config *buildconfig.Config, p *build.Path, hashFS *hashfs.HashFS, depsLog *ninjautil.DepsLog) (*Graph, error) {
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
	graph := &Graph{
		fname: fname,

		visited: make(map[*ninjautil.Edge]bool),

		globals: &globals{
			path:           p,
			hashFS:         hashFS,
			depsLog:        depsLog,
			buildConfig:    config,
			stepConfig:     stepConfig,
			replaces:       make(map[string][]string),
			accumulates:    make(map[string][]string),
			caseSensitives: make(map[string][]string),
			phony:          make(map[string]bool),
		},
	}
	err = graph.UpdateFilegroups(ctx)
	if err != nil {
		return nil, err
	}
	return graph, nil
}

// UpdateFilegroups updates filegroups.
func (g *Graph) UpdateFilegroups(ctx context.Context) error {
	return updateFilegroups(ctx, g.globals.buildConfig, g.globals.hashFS, g.globals.path, g.fname, g.globals.stepConfig)
}

func updateFilegroups(ctx context.Context, config *buildconfig.Config, hashFS *hashfs.HashFS, buildPath *build.Path, fname string, sc *StepConfig) error {
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
	ui.Default.PrintLines("update filegroups...")
	defer func() {
		ui.Default.PrintLines(fmt.Sprintf("update filegroups... %s\n", time.Since(started)))
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

func (g *Graph) load(ctx context.Context) error {
	started := time.Now()
	g.nstate = ninjautil.NewState()
	g.nstate.AddBinding("exec_root", g.globals.path.ExecRoot)
	g.nstate.AddBinding("working_directory", g.globals.path.Dir)
	p := ninjautil.NewManifestParser(g.nstate)
	spin := ui.Default.NewSpinner()
	spin.Start("loading %s...", g.fname)
	err := p.Load(ctx, g.fname)
	spin.Stop(nil)
	if err != nil {
		return fmt.Errorf("failed to load %s: %w", g.fname, err)
	}
	clog.Infof(ctx, "load %s %s", g.fname, time.Since(started))
	return nil
}

func (g *Graph) initGlobals() {
	// initialize caseSensitives.
	for _, f := range g.globals.stepConfig.CaseSensitiveInputs {
		cif := strings.ToLower(f)
		g.globals.caseSensitives[cif] = append(g.globals.caseSensitives[cif], f)
	}
}

func (g *Graph) init(ctx context.Context) error {
	g.once.Do(func() {
		err := g.load(ctx)
		if err != nil {
			g.initErr = err
			return
		}
		g.initGlobals()
	})
	return g.initErr
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
// Targets are exec-root relative paths.
func (g *Graph) Targets(ctx context.Context, args ...string) ([]string, error) {
	err := g.init(ctx)
	if err != nil {
		return nil, err
	}
	if len(args) == 0 {
		nodes, err := g.nstate.DefaultNodes()
		if err != nil {
			return nil, err
		}
		for _, n := range nodes {
			args = append(args, n.Path())
		}
	}
	targets := make([]string, 0, len(args))
	for _, t := range args {
		targets = append(targets, g.globals.path.MustFromWD(t))
	}
	return targets, nil
}

// StepDef creates new StepDef to build target (exec-root relative), needed for next.
// top-level target will use nil for next.
// It returns a StepDef for the target and inputs (exec-root relative).
func (g *Graph) StepDef(ctx context.Context, target string, next build.StepDef) (build.StepDef, []string, error) {
	n, ok := g.nstate.LookupNode(g.globals.path.MustToWD(target))
	if !ok {
		return nil, nil, build.ErrNoTarget
	}
	edge, ok := n.InEdge()
	if !ok {
		return nil, nil, build.ErrTargetIsSource
	}
	if g.visited[edge] {
		return nil, nil, build.ErrDuplicateStep
	}
	g.visited[edge] = true
	if edge.IsPhony() {
		g.globals.phony[target] = true
	}
	stepDef := g.newStepDef(ctx, edge, next)
	edgeInputs := edge.Inputs()
	inputs := make([]string, 0, len(edgeInputs))
	for _, in := range edgeInputs {
		inputs = append(inputs, g.globals.path.MustFromWD(in.Path()))
	}
	return stepDef, inputs, nil
}

// RecordDepsLog records deps log of output.
func (g *Graph) RecordDepsLog(ctx context.Context, output string, mtime time.Time, deps []string) (bool, error) {
	return g.globals.depsLog.Record(ctx, output, mtime, deps)
}

// InputDeps returns input deps.
func (g *Graph) InputDeps(ctx context.Context) map[string][]string {
	return g.globals.stepConfig.InputDeps
}
