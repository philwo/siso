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
	"sort"
	"strings"
	"time"

	"github.com/maruel/subcommands"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"

	"go.chromium.org/luci/common/cli"

	"infra/build/siso/build"
	"infra/build/siso/hashfs"
	fspb "infra/build/siso/hashfs/proto"
	"infra/build/siso/o11y/clog"
	"infra/build/siso/scandeps"
	pb "infra/build/siso/toolsupport/ciderutil/proto"
	"infra/build/siso/toolsupport/gccutil"
	"infra/build/siso/toolsupport/makeutil"
	"infra/build/siso/toolsupport/ninjautil"
	"infra/build/siso/toolsupport/shutil"
	"infra/build/siso/ui"
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
	started := time.Now()
	switch c.format {
	case "proto", "prototext", "json":
	default:
		return fmt.Errorf(`unknown format %q: "proto", "prototext" or "json": %w`, c.format, flag.ErrHelp)
	}
	analysis, err := c.analyze(ctx, args)
	if err != nil {
		analysis.Error = &pb.AnalysisError{
			ErrorMessage: err.Error(),
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
	fmt.Fprintf(os.Stderr, "siso query ideanalysis in %s\n", ui.FormatDuration(time.Since(started)))
	return nil
}

func (c *ideAnalysisRun) analyze(ctx context.Context, args []string) (*pb.IdeAnalysis, error) {
	analysis := &pb.IdeAnalysis{
		BuildOutDir: c.dir,
		WorkingDir:  c.dir,
	}
	if len(args) == 0 {
		return analysis, errors.New("no target given")
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
	analyzer := &ideAnalyzer{
		path: build.NewPath(execRoot, c.dir),
	}
	defer analyzer.Close(ctx)
	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		return analysis, err
	}
	analyzer.hashFS = hashFS

	eg, gctx := errgroup.WithContext(ctx)
	eg.Go(func() (finalErr error) {
		started := time.Now()
		defer func() {
			clog.Infof(gctx, "hashfs in %s: %v", time.Since(started), finalErr)
		}()
		fsstate, err := hashfs.Load(gctx, c.fsopt.StateFile)
		if err != nil {
			return err
		}
		// hashFS.SetState ?
		analyzer.fsm = hashfs.StateMap(fsstate)
		fmt.Fprintf(os.Stderr, "load hashfs state in %s\n", ui.FormatDuration(time.Since(started)))
		return nil
	})

	eg.Go(func() error {
		started := time.Now()
		analyzer.state = ninjautil.NewState()
		p := ninjautil.NewManifestParser(analyzer.state)
		err := p.Load(gctx, c.fname)
		clog.Infof(gctx, "ninja build in %s: %v", time.Since(started), err)
		fmt.Fprintf(os.Stderr, "load ninja build in %s\n", ui.FormatDuration(time.Since(started)))
		return err
	})
	err = eg.Wait()
	if err != nil {
		return analysis, err
	}
	analyzer.scanDeps = scandeps.New(hashFS, nil)
	buildableUnits := make(map[string]*pb.BuildableUnit)
	for _, arg := range args {
		result, bus := analyzer.analyzeTarget(ctx, arg)
		analysis.Results = append(analysis.Results, result)
		for k, v := range bus {
			buildableUnits[k] = v
		}
	}
	var buIDs []string
	for k := range buildableUnits {
		buIDs = append(buIDs, k)
	}
	sort.Strings(buIDs)
	for _, id := range buIDs {
		analysis.Units = append(analysis.Units, buildableUnits[id])
	}
	return analysis, nil
}

type ideAnalyzer struct {
	path     *build.Path
	hashFS   *hashfs.HashFS
	fsm      map[string]*fspb.Entry
	state    *ninjautil.State
	scanDeps *scandeps.ScanDeps
}

func (a *ideAnalyzer) Close(ctx context.Context) {
	clog.Infof(ctx, "close ideAnalys")
	err := a.hashFS.Close(ctx)
	if err != nil {
		clog.Warningf(ctx, "close hashFS: %v", err)
	}
}

func (a *ideAnalyzer) analyzeTarget(ctx context.Context, target string) (*pb.AnalysisResult, map[string]*pb.BuildableUnit) {
	result := &pb.AnalysisResult{}
	if strings.HasSuffix(target, "^") {
		result.SourceFilePath = strings.TrimSuffix(target, "^")
	}
	nodes, err := a.state.Targets([]string{target})
	if err != nil {
		result.Status = &pb.AnalysisResult_Status{
			Code:          pb.AnalysisResult_Status_CODE_NOT_FOUND,
			StatusMessage: proto.String(err.Error()),
		}
		return result, nil
	}
	if len(nodes) != 1 {
		result.Status = &pb.AnalysisResult_Status{
			Code:          pb.AnalysisResult_Status_CODE_NOT_FOUND,
			StatusMessage: proto.String(fmt.Sprintf("target=%q expands to %d nodes", target, len(nodes))),
		}
		return result, nil
	}
	node := nodes[0]
	inEdge, ok := node.InEdge()
	if !ok {
		result.Status = &pb.AnalysisResult_Status{
			Code:          pb.AnalysisResult_Status_CODE_NOT_FOUND,
			StatusMessage: proto.String(fmt.Sprintf("no input edge for %s", node.Path())),
		}
		return result, nil
	}
	if len(inEdge.Inputs()) == 0 {
		result.Status = &pb.AnalysisResult_Status{
			Code:          pb.AnalysisResult_Status_CODE_NOT_FOUND,
			StatusMessage: proto.String(fmt.Sprintf("no inputs for %s", node.Path())),
		}
		return result, nil
	}
	if result.SourceFilePath == "" {
		result.SourceFilePath = inEdge.Inputs()[0].Path()
	}

	switch filepath.Ext(result.SourceFilePath) {
	case ".c", ".cc", ".cxx", ".cpp", ".m", ".mm", ".S", ".h", ".hxx", ".hpp", ".inc":
		return a.analyzeCPP(ctx, inEdge, result)
	case ".java":
		return a.analyzeJava(ctx, inEdge, result)
	default:
		clog.Infof(ctx, "analyze unknown %s", result.SourceFilePath)
	}
	// TODO: what should we return?
	return result, nil
}

func (a *ideAnalyzer) analyzeCPP(ctx context.Context, edge *ninjautil.Edge, result *pb.AnalysisResult) (*pb.AnalysisResult, map[string]*pb.BuildableUnit) {
	clog.Infof(ctx, "analyze cpp %s", result.SourceFilePath)
	deps := map[string]*pb.BuildableUnit{}

	// get command line from edge
	command := edge.Binding("command")
	cmdArgs, err := shutil.Split(command)
	if err != nil {
		result.Status = &pb.AnalysisResult_Status{
			Code:          pb.AnalysisResult_Status_CODE_BUILD_FAILED,
			StatusMessage: proto.String(fmt.Sprintf("failed to parse command=%q: %v", command, err)),
		}
		return result, nil
	}
	// scandeps
	params := gccutil.ExtractScanDepsParams(ctx, cmdArgs, nil)
	for i := range params.Sources {
		params.Sources[i] = a.path.MaybeFromWD(ctx, params.Sources[i])
	}
	// no need to canonicalize path for Includes.
	// it should be used as is for `#include "pathname.h"`
	for i := range params.Files {
		params.Files[i] = a.path.MaybeFromWD(ctx, params.Files[i])
	}
	for i := range params.Dirs {
		params.Dirs[i] = a.path.MaybeFromWD(ctx, params.Dirs[i])
	}
	for i := range params.Frameworks {
		params.Frameworks[i] = a.path.MaybeFromWD(ctx, params.Frameworks[i])
	}
	for i := range params.Sysroots {
		params.Sysroots[i] = a.path.MaybeFromWD(ctx, params.Sysroots[i])
	}
	req := scandeps.Request{
		Defines:    params.Defines,
		Sources:    params.Sources,
		Includes:   params.Includes,
		Dirs:       params.Dirs,
		Frameworks: params.Frameworks,
		Sysroots:   params.Sysroots,
	}
	started := time.Now()
	clog.Infof(ctx, "scandeps %#v", req)
	incs, err := a.scanDeps.Scan(ctx, a.path.ExecRoot, req)
	if err != nil {
		result.Status = &pb.AnalysisResult_Status{
			Code:          pb.AnalysisResult_Status_CODE_BUILD_FAILED,
			StatusMessage: proto.String(fmt.Sprintf("failed to scandeps %#v: %v", req, err)),
		}
		return result, nil
	}
	clog.Infof(ctx, "scandeps results: %q", incs)
	fmt.Fprintf(os.Stderr, "%s scandeps in %s\n", result.SourceFilePath, ui.FormatDuration(time.Since(started)))
	started = time.Now()

	for _, inc := range incs {
		incTarget := a.path.MaybeToWD(ctx, inc)
		node, ok := a.state.LookupNodeByPath(incTarget)
		if !ok {
			clog.Infof(ctx, "not in build graph: %s", incTarget)
			continue
		}
		inEdge, ok := node.InEdge()
		if !ok {
			clog.Infof(ctx, "not generated file: %s", incTarget)
			continue
		}
		// for each generated files
		var generatedFiles []*pb.GeneratedFile
		for _, out := range inEdge.Outputs() {
			path := out.Path()
			buf, err := a.hashFS.ReadFile(ctx, a.path.ExecRoot, a.path.MaybeFromWD(ctx, path))
			if err != nil {
				clog.Infof(ctx, "not exist generated file %q: %v", path, err)
				continue
			}
			generatedFiles = append(generatedFiles, &pb.GeneratedFile{
				Path:     path,
				Contents: buf,
			})
		}
		//   add buildable unit for the inEdge
		deps[incTarget] = a.buildableUnit(ctx, inEdge, pb.Language_LANGUAGE_UNSPECIFIED, generatedFiles, nil)
	}
	buildableUnit := a.buildableUnit(ctx, edge, pb.Language_LANGUAGE_CPP, nil, deps)
	result.UnitId = buildableUnit.Id
	result.Invalidation = a.invalidation(ctx)

	deps[buildableUnit.Id] = buildableUnit
	result.Status = &pb.AnalysisResult_Status{
		Code: pb.AnalysisResult_Status_CODE_OK,
	}
	fmt.Fprintf(os.Stderr, "%s analysis result in %s\n", result.SourceFilePath, ui.FormatDuration(time.Since(started)))
	return result, deps
}

func (a *ideAnalyzer) analyzeJava(ctx context.Context, edge *ninjautil.Edge, result *pb.AnalysisResult) (*pb.AnalysisResult, map[string]*pb.BuildableUnit) {
	// TODO: implement this
	clog.Infof(ctx, "analyze java %s", result.SourceFilePath)
	return result, nil
}

func (a *ideAnalyzer) buildableUnit(ctx context.Context, edge *ninjautil.Edge, language pb.Language, outputs []*pb.GeneratedFile, deps map[string]*pb.BuildableUnit) *pb.BuildableUnit {
	outPath := edge.Outputs()[0].Path()
	clog.Infof(ctx, "buildableUnit for %s: lang=%s", outPath, language)

	var depIDs []string
	for k := range deps {
		depIDs = append(depIDs, k)
	}
	sort.Strings(depIDs)

	seen := make(map[string]bool)
	var sourceFiles []string
	for _, in := range edge.Inputs() {
		if seen[in.Path()] {
			continue
		}
		seen[in.Path()] = true
		_, ok := in.InEdge()
		if ok {
			clog.Infof(ctx, "generated source file: %s", in.Path())
			continue
		}
		sourceFiles = append(sourceFiles, in.Path())
	}

	command := edge.Binding("command")
	cmdArgs, err := shutil.Split(command)
	if err != nil {
		cmdArgs = []string{"/bin/sh", "-c", command}
	}

	return &pb.BuildableUnit{
		Id:                outPath,
		Language:          language,
		SourceFilePaths:   sourceFiles,
		CompilerArguments: cmdArgs,
		GeneratedFiles:    outputs,
		DependencyIds:     depIDs,
	}
}

func (a *ideAnalyzer) invalidation(ctx context.Context) *pb.Invalidation {
	inv := &pb.Invalidation{
		Wildcards: []*pb.Invalidation_Wildcard{
			{
				Suffix:         proto.String(".gn"),
				CanCrossFolder: proto.Bool(true),
			},
			{
				Suffix:         proto.String(".gni"),
				CanCrossFolder: proto.Bool(true),
			},
		},
	}
	buf, err := a.hashFS.ReadFile(ctx, a.path.ExecRoot, filepath.Join(a.path.Dir, "build.ninja.d"))
	if err != nil {
		clog.Warningf(ctx, "failed to read build.ninja.d: %v", err)
		return inv
	}
	bdeps, err := makeutil.ParseDeps(buf)
	if err != nil {
		clog.Warningf(ctx, "failed to parse build.ninja.d: %v", err)
		return inv
	}
	for _, d := range bdeps {
		switch filepath.Ext(d) {
		case ".gn", ".gni":
			continue
		}
		// ignore generated files?
		// e.g. *.build_metadata, *.typemap_config etc
		if filepath.IsLocal(d) {
			continue
		}
		inv.FilePaths = append(inv.FilePaths, filepath.ToSlash(filepath.Join(a.path.Dir, d)))
	}
	return inv
}
