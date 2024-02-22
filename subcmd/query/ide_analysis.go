// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package query

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/maruel/subcommands"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"

	"go.chromium.org/luci/common/cli"

	"infra/build/siso/hashfs"
	fspb "infra/build/siso/hashfs/proto"
	"infra/build/siso/o11y/clog"
	pb "infra/build/siso/toolsupport/ciderutil/proto"
	"infra/build/siso/toolsupport/ninjautil"
	"infra/build/siso/toolsupport/shutil"
)

// go/reqs-for-peep

type ideAnalysisRun struct {
	subcommands.CommandRunBase

	dir         string
	fname       string
	fsopt       *hashfs.Option
	depsLogFile string
	format      string
}

const ideAnalysisUsage = `query ninja build graph for Cider-G.

 $ siso query ideanalysis -C <dir> [--format <format>] [targets...]

format: proto, prototext or json
`

func cmdIDEAnalysis() *subcommands.Command {
	return &subcommands.Command{
		UsageLine: "ideanalysis",
		ShortDesc: "query ninja build graph ofor Cider-G",
		LongDesc:  ideAnalysisUsage,
		CommandRun: func() subcommands.CommandRun {
			c := &ideAnalysisRun{}
			c.init()
			return c
		},
	}
}

func (c *ideAnalysisRun) init() {
	c.Flags.StringVar(&c.dir, "C", ".", "ninja running directory to find build.ninja")
	c.Flags.StringVar(&c.fname, "f", "build.ninja", "input build filename (relative to -C)")
	c.fsopt = new(hashfs.Option)
	c.fsopt.StateFile = ".siso_fs_state"
	c.fsopt.RegisterFlags(&c.Flags)
	c.Flags.StringVar(&c.depsLogFile, "deps_log", ".siso_deps", "deps log filename (relative to -C)")
	c.Flags.StringVar(&c.format, "format", "proto", `output format. "proto", "prototext" or "json"`)
}

func (c *ideAnalysisRun) Run(a subcommands.Application, args []string, env subcommands.Env) int {
	ctx := cli.GetContext(a, c, env)
	err := c.run(ctx, args)
	if err != nil {
		switch {
		case errors.Is(err, flag.ErrHelp):
			fmt.Fprintf(os.Stderr, "%v\n%s\n", err, ideAnalysisUsage)
		default:
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		return 1
	}
	return 0
}

func (c *ideAnalysisRun) run(ctx context.Context, args []string) error {
	switch c.format {
	case "proto", "prototext", "json":
	default:
		return fmt.Errorf(`unknown format %q: "proto", "prototext" or "json": %w`, c.format, flag.ErrHelp)
	}
	analysis, err := c.analysis(ctx, args)
	if err != nil {
		analysis.Status = &pb.Status{
			Code:    pb.Status_FAILURE,
			Message: proto.String(err.Error()),
		}
	} else {
		analysis.Status = &pb.Status{
			Code: pb.Status_OK,
		}
	}
	var buf []byte
	switch c.format {
	case "proto":
		buf, err = proto.Marshal(analysis)
	case "prototext":
		buf, err = prototext.MarshalOptions{
			Multiline: true,
			Indent:    " ",
		}.Marshal(analysis)
	case "json":
		buf, err = json.MarshalIndent(analysis, "", " ")
	}
	if err != nil {
		return err
	}
	fmt.Printf("%s", buf)
	return nil
}

func (c *ideAnalysisRun) analysis(ctx context.Context, args []string) (*pb.IdeAnalysis, error) {
	analysis := &pb.IdeAnalysis{
		BuildArtifactRoot: c.dir,
	}
	// TODO: use ninja's initWorkdirs?

	// don't use $PWD for current directory
	// to avoid symlink issue. b/286779149
	pwd := os.Getenv("PWD")
	_ = os.Unsetenv("PWD") // no error for safe env key name.

	execRoot, err := os.Getwd()
	if pwd != "" {
		_ = os.Setenv("PWD", pwd) // no error to reset env with valid value.
	}
	if err != nil {
		return analysis, err
	}

	err = os.Chdir(c.dir)
	if err != nil {
		return analysis, err
	}
	var depsLog *ninjautil.DepsLog
	var fsm map[string]*fspb.Entry
	var state *ninjautil.State
	defer func() {
		if depsLog == nil {
			return
		}
		err := depsLog.Close()
		if err != nil {
			clog.Warningf(ctx, "close depslog: %v", err)
		}
	}()
	eg, gctx := errgroup.WithContext(ctx)
	eg.Go(func() error {
		started := time.Now()
		var err error
		depsLog, err = ninjautil.NewDepsLog(gctx, c.depsLogFile)
		clog.Infof(gctx, "depslog in %s: %v", time.Since(started), err)
		return err
	})
	eg.Go(func() (finalErr error) {
		started := time.Now()
		defer func() {
			clog.Infof(gctx, "hashfs in %s: %v", time.Since(started), finalErr)
		}()
		fsstate, err := hashfs.Load(gctx, c.fsopt.StateFile)
		if err != nil {
			return err
		}
		fsm = hashfs.StateMap(fsstate)
		return nil
	})

	eg.Go(func() error {
		started := time.Now()
		state = ninjautil.NewState()
		p := ninjautil.NewManifestParser(state)
		err := p.Load(gctx, c.fname)
		clog.Infof(gctx, "ninja build in %s: %v", time.Since(started), err)
		return err
	})
	err = eg.Wait()
	if err != nil {
		return analysis, err
	}

	nodes, err := state.Targets(args)
	if err != nil {
		return analysis, err
	}
	for _, node := range nodes {
		source := &pb.SourceFile{}
		inEdge, ok := node.InEdge()
		if !ok {
			source.Status = &pb.Status{
				Code:    pb.Status_FAILURE,
				Message: proto.String(fmt.Sprintf("no input edge for %s", node.Path())),
			}
			analysis.Sources = append(analysis.Sources, source)
			continue

		}
		if len(inEdge.Inputs()) == 0 {
			source.Status = &pb.Status{
				Code:    pb.Status_FAILURE,
				Message: proto.String(fmt.Sprintf("no inputs for %s", node.Path())),
			}
			analysis.Sources = append(analysis.Sources, source)
			continue
		}
		// better to choose source that match args without ^?
		sourceNode := inEdge.Inputs()[0]
		// sourceNode.Path is working dir relative,
		// but source.Path should be repo root relative.
		source.Path = filepath.ToSlash(filepath.Join(c.dir, sourceNode.Path()))
		source.WorkingDir = c.dir
		outEdges := sourceNode.OutEdges()
		if len(outEdges) == 0 {
			analysis.Sources = append(analysis.Sources, source)
			continue
		}
		outEdge := outEdges[0]
		var out string
		if len(outEdge.Outputs()) > 0 {
			node := outEdge.Outputs()[0]
			out = node.Path()
		}
		seen := make(map[string]bool)

		addInputs := func(fname string) {
			// fname is cur dir relative
			// i.e. build_artifact_root relative.
			if seen[fname] {
				return
			}
			seen[fname] = true
			generated := &pb.GeneratedFile{
				Path: fname,
			}
			fullpath := filepath.Join(execRoot, c.dir, fname)
			ent, ok := fsm[fullpath]
			if ok && len(ent.CmdHash) == 0 {
				// source file
				clog.Infof(ctx, "%s %s: not generated", source.Path, fname)
				return
			}
			source.Deps = append(source.Deps, c.getDeps(ctx, state, fsm, execRoot, fname)...)
			// TODO: fetch content from RBE-CAS by digest if needed.
			buf, err := os.ReadFile(fullpath)
			if err != nil {
				clog.Warningf(ctx, "%s %s: failed to get contents: %v", source.Path, fname, err)
				// report it with empty contents.
				source.Generated = append(source.Generated, generated)
				return
			}
			generated.Contents = buf
			source.Generated = append(source.Generated, generated)
		}
		if rspfile := outEdge.UnescapedBinding("rspfile"); rspfile != "" {
			rspfileContent := outEdge.Binding("rspfile_content")
			generated := &pb.GeneratedFile{
				Path:     rspfile,
				Contents: []byte(rspfileContent),
			}
			source.Generated = append(source.Generated, generated)
		}
		depsLogUsed := false
		if out != "" {
			deps, _, err := depsLog.Get(ctx, out)
			if err != nil {
				clog.Warningf(ctx, "deps %s: %v", out, err)
			} else {
				for _, in := range deps {
					addInputs(in)
				}
				// If deps is available, check generated from deps only.
				depsLogUsed = true
			}
		}
		if !depsLogUsed {
			for _, node := range outEdge.Inputs() {
				addInputs(node.Path())
			}
		}

		command := outEdge.Binding("command")
		cmdArgs, err := shutil.Split(command)
		if err != nil {
			cmdArgs = []string{"/bin/sh", "-c", command}
		}
		source.CompilerArguments = cmdArgs
		// TODO: add deps that would change compiler arguments?
		slices.Sort(source.Deps)
		source.Deps = slices.Compact(source.Deps)

		source.Status = &pb.Status{
			Code: pb.Status_OK,
		}
		analysis.Sources = append(analysis.Sources, source)

	}
	return analysis, nil
}

// getDeps returns deps for fname (generated file).
// fname is working dir relative, but deps are repo root relative.
// e.g. *.proto for *.pb.h
// ref: http://shortn/_GgVRbmzqOx
// TOOD: could be chromium specific. use starlark to support various cases?
func (c *ideAnalysisRun) getDeps(ctx context.Context, state *ninjautil.State, fsm map[string]*fspb.Entry, execRoot, fname string) []string {
	// by default, all source inputs of generated file as deps.
	filter := func(in *ninjautil.Node) (string, bool) {
		fname := in.Path()
		fullpath := filepath.Join(execRoot, c.dir, fname)
		ent, ok := fsm[fullpath]
		if ok && len(ent.CmdHash) > 0 {
			// generated file
			// TODO: better to collect all indirect source input?
			return "", false
		}
		return filepath.ToSlash(filepath.Join(c.dir, fname)), true
	}
	switch {
	case strings.HasSuffix(fname, ".pb.h"):
		// use *.proto for *.pb.h
		filter = func(in *ninjautil.Node) (string, bool) {
			fname := in.Path()
			if !strings.HasSuffix(fname, ".proto") {
				return "", false
			}
			fullpath := filepath.Join(execRoot, c.dir, fname)
			ent, ok := fsm[fullpath]
			if ok && len(ent.CmdHash) > 0 {
				// generated file
				// TODO: recursively check input *.proto, or its source?
				return "", false
			}
			return filepath.ToSlash(filepath.Join(c.dir, fname)), true
		}
	}
	node, ok := state.LookupNode(fname)
	if !ok {
		clog.Warningf(ctx, "not found %s in build graph", fname)
		return nil
	}
	edge, ok := node.InEdge()
	if !ok {
		clog.Warningf(ctx, "no in-edge for %s", fname)
		return nil
	}
	// edge is proto gen
	var deps []string
	for _, in := range edge.Inputs() {
		dep, ok := filter(in)
		if ok {
			deps = append(deps, dep)
		}
	}
	return deps
}
