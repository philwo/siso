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
	"time"

	"github.com/maruel/subcommands"
	"golang.org/x/sync/errgroup"

	"go.chromium.org/luci/common/cli"

	"infra/build/siso/hashfs"
	pb "infra/build/siso/hashfs/proto"
	"infra/build/siso/o11y/clog"
	"infra/build/siso/toolsupport/ninjautil"
	"infra/build/siso/toolsupport/shutil"
)

// go/reqs-for-peep
// TODO: use protobuf

type IDEAnalysis struct {
	BuildArtifactRoot string       `json:"build_artifact_root"`
	Sources           []SourceFile `json:"sources"`
	Status            Status       `json:"status"`
}

type SourceFile struct {
	Path              string          `json:"path"`
	WorkingDir        string          `json:"working_dir"`
	CompilerArguments []string        `json:"compiler_arguments"`
	Generated         []GeneratedFile `json:"generated,omitempty"`
	Deps              []string        `json:"deps,omitempty"`
	Status            Status          `json:"status"`
}

type GeneratedFile struct {
	Path     string `json:"path"`
	Contents string `json:"contents,omitempty"` // []byte?
}

type Code string

const (
	Ok      Code = "OK"
	Failure Code = "FAILURE"
)

type Status struct {
	Code    Code   `json:"code"`
	Message string `json:"message,omitempty"`
}

type ideAnalysisRun struct {
	subcommands.CommandRunBase

	dir         string
	fname       string
	fsopt       *hashfs.Option
	depsLogFile string
}

const ideAnalysisUsage = `query ninja build graph for Cider-G.

 $ siso query ideanalysis -C <dir> [targets...]

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
	analysis, err := c.analysis(ctx, args)
	if err != nil {
		analysis.Status.Code = Failure
		analysis.Status.Message = err.Error()
	} else {
		analysis.Status.Code = Ok
	}
	buf, err := json.MarshalIndent(analysis, "", " ")
	if err != nil {
		return err
	}
	fmt.Printf("%s\n", buf)
	return nil
}

func (c *ideAnalysisRun) analysis(ctx context.Context, args []string) (IDEAnalysis, error) {
	analysis := IDEAnalysis{
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
	var fsm map[string]*pb.Entry
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
		var source SourceFile
		// source.Path is working dir relative
		// as compilation database.
		// TODO: go/reqs-for-peep says it is repository root relative
		inEdge, ok := node.InEdge()
		if !ok {
			source.Status = Status{
				Code:    Failure,
				Message: fmt.Sprintf("no input edge for %s", node.Path()),
			}
			analysis.Sources = append(analysis.Sources, source)
			continue

		}
		if len(inEdge.Inputs()) == 0 {
			source.Status = Status{
				Code:    Failure,
				Message: fmt.Sprintf("no inputs for %s", node.Path()),
			}
			analysis.Sources = append(analysis.Sources, source)
			continue
		}
		// better to choose source that match args without ^?
		sourceNode := inEdge.Inputs()[0]
		source.Path = sourceNode.Path()
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
		addInputs := func(fname string, needGenerated bool) {
			if seen[fname] {
				return
			}
			seen[fname] = true
			source.Deps = append(source.Deps, fname)
			if !needGenerated {
				return
			}
			generated := GeneratedFile{
				Path: fname,
			}
			fullpath := filepath.Join(execRoot, c.dir, fname)
			ent, ok := fsm[fullpath]
			if !ok {
				clog.Infof(ctx, "%s %s: not in hashfs entry %s", source.Path, fname, fullpath)
				// generated file, but not yet generated?
				source.Generated = append(source.Generated, generated)
				return
			}
			if len(ent.CmdHash) == 0 {
				// source file
				clog.Infof(ctx, "%s %s: not generated", source.Path, fname)
				return
			}
			// TODO: fetch content from RBE-CAS by digest if needed.
			buf, err := os.ReadFile(fullpath)
			if err != nil {
				clog.Warningf(ctx, "%s %s: failed to get contents: %v", source.Path, fname, err)
				// report it with empty contents.
				source.Generated = append(source.Generated, generated)
				return
			}
			generated.Contents = string(buf)
			source.Generated = append(source.Generated, generated)
		}
		if rspfile := outEdge.UnescapedBinding("rspfile"); rspfile != "" {
			rspfileContent := outEdge.Binding("rspfile_content")
			generated := GeneratedFile{
				Path:     rspfile,
				Contents: rspfileContent,
			}
			source.Generated = append(source.Generated, generated)
		}
		needGenerated := true
		if out != "" {
			deps, _, err := depsLog.Get(ctx, out)
			if err != nil {
				clog.Warningf(ctx, "deps %s: %v", out, err)
			} else {
				for _, in := range deps {
					addInputs(in, needGenerated)
				}
				// If deps is available, check generated from deps only.
				needGenerated = false
			}
		}
		for _, node := range outEdge.Inputs() {
			addInputs(node.Path(), needGenerated)
		}

		command := outEdge.Binding("command")
		cmdArgs, err := shutil.Split(command)
		if err != nil {
			cmdArgs = []string{"/bin/sh", "-c", command}
		}
		source.CompilerArguments = cmdArgs

		source.Status.Code = Ok
		analysis.Sources = append(analysis.Sources, source)

	}
	return analysis, nil
}
