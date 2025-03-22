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
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/log"
	"github.com/maruel/subcommands"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"

	"go.chromium.org/luci/common/cli"

	"go.chromium.org/infra/build/siso/build"
	"go.chromium.org/infra/build/siso/hashfs"
	fspb "go.chromium.org/infra/build/siso/hashfs/proto"
	"go.chromium.org/infra/build/siso/scandeps"
	pb "go.chromium.org/infra/build/siso/toolsupport/ciderutil/proto"
	"go.chromium.org/infra/build/siso/toolsupport/gccutil"
	"go.chromium.org/infra/build/siso/toolsupport/makeutil"
	"go.chromium.org/infra/build/siso/toolsupport/ninjautil"
	"go.chromium.org/infra/build/siso/toolsupport/shutil"
	"go.chromium.org/infra/build/siso/ui"
)

// go/reqs-for-peep

type ideAnalysisRun struct {
	subcommands.CommandRunBase

	execRoot string
	dir      string
	fname    string
	fsopt    *hashfs.Option
	format   string
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

	// TODO: use ninja's initWorkdirs?

	// don't use $PWD for current directory
	// to avoid symlink issue. b/286779149
	pwd := os.Getenv("PWD")
	_ = os.Unsetenv("PWD") // no error for safe env key name.

	var err error
	c.execRoot, err = os.Getwd()
	if pwd != "" {
		_ = os.Setenv("PWD", pwd) // no error to reset env with valid value.
	}
	if err != nil {
		return err
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
	err := os.Chdir(c.dir)
	if err != nil {
		return analysis, err
	}
	analyzer := &ideAnalyzer{
		path: build.NewPath(c.execRoot, c.dir),
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
		fsstate, err := hashfs.Load(hashfs.Option{StateFile: c.fsopt.StateFile})
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
		maps.Copy(buildableUnits, bus)
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
	err := a.hashFS.Close(ctx)
	if err != nil {
		log.Warnf("close hashFS: %v", err)
	}
}

func (a *ideAnalyzer) analyzeTarget(ctx context.Context, target string) (*pb.AnalysisResult, map[string]*pb.BuildableUnit) {
	result := &pb.AnalysisResult{}
	if strings.HasSuffix(target, "^") {
		result.SourceFilePath = strings.TrimSuffix(target, "^")
	}
	nodes, err := a.targetForCPP(target)
	if err != nil {
		result.Status = &pb.AnalysisResult_Status{
			Code:          pb.AnalysisResult_Status_CODE_NOT_FOUND,
			StatusMessage: proto.String(err.Error()),
		}
		return result, nil
	}
	if len(nodes) == 0 {
		result.Status = &pb.AnalysisResult_Status{
			Code:          pb.AnalysisResult_Status_CODE_NOT_FOUND,
			StatusMessage: proto.String(fmt.Sprintf("missing target=%q", target)),
		}
		return result, nil
	}
	node := nodes[0]

	var nodeEnt *fspb.Entry
	// need to check build succeeded for .java
	// for cxx, we don't compile *.o with
	// `SISO_EXPERIMENTS=prepare-header-only`, so *.o may not exist.
	if filepath.Ext(result.SourceFilePath) == ".java" {
		var ok bool
		nodeEnt, ok = a.fsm[filepath.ToSlash(filepath.Join(a.path.ExecRoot, a.path.Dir, node.Path()))]
		if !ok {
			result.Status = &pb.AnalysisResult_Status{
				Code:          pb.AnalysisResult_Status_CODE_BUILD_FAILED,
				StatusMessage: proto.String(fmt.Sprintf("file not found: %q", node.Path())),
			}
			return result, nil
		}
	}

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
	for _, input := range inEdge.Inputs() {
		e, ok := input.InEdge()
		if ok && e.IsPhony() {
			// TODO: check phony's inputs?
			continue
		}
		ent, ok := a.fsm[filepath.ToSlash(filepath.Join(a.path.ExecRoot, a.path.Dir, input.Path()))]
		if !ok {
			result.Status = &pb.AnalysisResult_Status{
				Code:          pb.AnalysisResult_Status_CODE_BUILD_FAILED,
				StatusMessage: proto.String(fmt.Sprintf("input of %q not found: %q", node.Path(), input.Path())),
			}
			return result, nil
		}
		if nodeEnt != nil && nodeEnt.GetUpdatedTime() < ent.GetId().GetModTime() {
			result.Status = &pb.AnalysisResult_Status{
				Code:          pb.AnalysisResult_Status_CODE_BUILD_FAILED,
				StatusMessage: proto.String(fmt.Sprintf("target is not up-to-date: %q(%d) is older than %q(%d)", node.Path(), nodeEnt.GetUpdatedTime(), input.Path(), ent.GetId().GetModTime())),
			}
			return result, nil
		}
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
		log.Warnf("analyze unknown %s", result.SourceFilePath)
	}
	// TODO: what should we return?
	return result, nil
}

func (a *ideAnalyzer) targetForCPP(target string) ([]*ninjautil.Node, error) {
	nodes, err := a.state.Targets([]string{target})
	if err == nil {
		return nodes, nil
	}

	// err != nil
	switch filepath.Ext(target) {
	case ".h^", ".hxx^", ".hpp^", ".inc^":
		hname := strings.TrimSuffix(target, "^")
		_, serr := os.Stat(hname)
		if serr != nil {
			log.Warnf("file not exist: %v", serr)
			return nil, err
		}
		// fallback to any *.cc file in the directory.
		// language service need to know include directories and
		// flags that affects compiler's behavior, but doesn't care
		// *.cc includes given header.
		dirname := filepath.Dir(target)
		dirents, derr := os.ReadDir(dirname)
		if derr != nil {
			log.Warnf("readdir %q: %v", dirname, derr)
			return nil, err
		}
		files := make(map[string]*ninjautil.Node)
		for _, dirent := range dirents {
			fname := dirent.Name()
			switch filepath.Ext(fname) {
			case ".c", ".cc", "cxx", ".cpp", ".m", ".mm", ".S":
				fpath := filepath.ToSlash(filepath.Join(dirname, fname))
				nodes, err := a.state.Targets([]string{fpath + "^"})
				if err != nil {
					continue
				}
				files[fpath] = nodes[0]
			}
		}
		if len(files) == 0 {
			return nil, fmt.Errorf("no candidate in %s: %w", dirname, err)
		}
		var filenames []string
		for fname := range files {
			filenames = append(filenames, fname)
		}
		sort.Strings(filenames)
		fmt.Fprintf(os.Stderr, "target %q not found: fallback to %q\n", target, filenames[0]+"^")
		return []*ninjautil.Node{files[filenames[0]]}, nil
	}
	return nil, err
}

func (a *ideAnalyzer) analyzeCPP(ctx context.Context, edge *ninjautil.Edge, result *pb.AnalysisResult) (*pb.AnalysisResult, map[string]*pb.BuildableUnit) {
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
	params := gccutil.ExtractScanDepsParams(cmdArgs, nil)
	for i := range params.Sources {
		params.Sources[i] = a.path.MaybeFromWD(params.Sources[i])
	}
	// no need to canonicalize path for Includes.
	// it should be used as is for `#include "pathname.h"`
	for i := range params.Files {
		params.Files[i] = a.path.MaybeFromWD(params.Files[i])
	}
	for i := range params.Dirs {
		params.Dirs[i] = a.path.MaybeFromWD(params.Dirs[i])
	}
	for i := range params.Frameworks {
		params.Frameworks[i] = a.path.MaybeFromWD(params.Frameworks[i])
	}
	for i := range params.Sysroots {
		params.Sysroots[i] = a.path.MaybeFromWD(params.Sysroots[i])
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
	incs, err := a.scanDeps.Scan(ctx, a.path.ExecRoot, req)
	if err != nil {
		result.Status = &pb.AnalysisResult_Status{
			Code:          pb.AnalysisResult_Status_CODE_BUILD_FAILED,
			StatusMessage: proto.String(fmt.Sprintf("failed to scandeps %#v: %v", req, err)),
		}
		return result, nil
	}
	fmt.Fprintf(os.Stderr, "%s scandeps in %s\n", result.SourceFilePath, ui.FormatDuration(time.Since(started)))
	started = time.Now()

	for _, inc := range incs {
		incTarget := a.path.MaybeToWD(inc)
		node, ok := a.state.LookupNodeByPath(incTarget)
		if !ok {
			continue
		}
		inEdge, ok := node.InEdge()
		if !ok {
			continue
		}
		// for each generated files
		var generatedFiles []*pb.GeneratedFile
		for _, out := range inEdge.Outputs() {
			path := out.Path()
			buf, err := a.hashFS.ReadFile(ctx, a.path.ExecRoot, a.path.MaybeFromWD(path))
			if err != nil {
				continue
			}
			generatedFiles = append(generatedFiles, &pb.GeneratedFile{
				Path:     path,
				Contents: buf,
			})
		}
		// add buildable unit for the inEdge
		bu := a.buildableUnit(inEdge, pb.Language_LANGUAGE_UNSPECIFIED, edgeCmdArgs(inEdge), generatedFiles, nil)
		deps[bu.Id] = bu
	}
	depIDs := make([]string, 0, len(deps))
	for k := range deps {
		depIDs = append(depIDs, k)
	}
	sort.Strings(depIDs)
	buildableUnit := a.buildableUnit(edge, pb.Language_LANGUAGE_CPP, cmdArgs, nil, depIDs)
	result.UnitId = buildableUnit.Id
	result.Invalidation = a.invalidation(ctx)

	deps[buildableUnit.Id] = buildableUnit
	result.Status = &pb.AnalysisResult_Status{
		Code: pb.AnalysisResult_Status_CODE_OK,
	}
	fmt.Fprintf(os.Stderr, "%s analysis result in %s\n", result.SourceFilePath, ui.FormatDuration(time.Since(started)))
	return result, deps
}

func edgeCmdArgs(edge *ninjautil.Edge) []string {
	command := edge.Binding("command")
	cmdArgs, err := shutil.Split(command)
	if err != nil {
		cmdArgs = []string{"/bin/sh", "-c", command}
	}
	return cmdArgs
}

func (a *ideAnalyzer) buildableUnit(edge *ninjautil.Edge, language pb.Language, cmdArgs []string, outputs []*pb.GeneratedFile, depIDs []string) *pb.BuildableUnit {
	outPath := edge.Outputs()[0].Path()

	seen := make(map[string]bool)
	var sourceFiles []string
	for _, in := range edge.Inputs() {
		if seen[in.Path()] {
			continue
		}
		seen[in.Path()] = true
		_, ok := in.InEdge()
		if ok {
			continue
		}
		switch language {
		case pb.Language_LANGUAGE_JAVA:
			switch filepath.Ext(in.Path()) {
			case ".java", ".class":
			case ".jar":
				// precomputed jar files are represented
				// as a buildable unit to reduce the size
				// of computed index b/392977625
				continue
			default:
				continue
			}
		}
		sourceFiles = append(sourceFiles, in.Path())
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
		log.Warnf("failed to read build.ninja.d: %v", err)
		return inv
	}
	bdeps, err := makeutil.ParseDeps(buf)
	if err != nil {
		log.Warnf("failed to parse build.ninja.d: %v", err)
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
