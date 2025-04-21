// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package ninja implements the subcommand `ninja` which parses a `build.ninja` file and builds the requested targets.
package ninja

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/log"
	"github.com/klauspost/cpuid/v2"
	"golang.org/x/oauth2"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"go.chromium.org/luci/cipd/version"

	"go.chromium.org/infra/build/siso/auth/cred"
	"go.chromium.org/infra/build/siso/build"
	"go.chromium.org/infra/build/siso/build/buildconfig"
	"go.chromium.org/infra/build/siso/build/cachestore"
	"go.chromium.org/infra/build/siso/build/ninjabuild"
	"go.chromium.org/infra/build/siso/hashfs"
	"go.chromium.org/infra/build/siso/reapi"
	"go.chromium.org/infra/build/siso/reapi/digest"
	"go.chromium.org/infra/build/siso/subcmd/ninja/ninjalog"
	"go.chromium.org/infra/build/siso/toolsupport/ninjautil"
	"go.chromium.org/infra/build/siso/toolsupport/soongutil"
	"go.chromium.org/infra/build/siso/ui"
)

// File name of siso metadata file.
// This file is read by ninjalog_uploader.py, in order to populate metadata.
const sisoMetadataFilename = ".siso_metadata.json"

const ninjaUsage = `build the requested targets as ninja.

 $ siso ninja [-C <dir>] [options] [targets...]

`

type NinjaOpts struct {
	Ts      oauth2.TokenSource
	version string
	started time.Time

	// flag values
	Dir        string
	ConfigName string
	ProjectID  string

	Offline    bool
	DryRun     bool
	Clobber    bool
	ActionSalt string

	RemoteJobs int
	Fname      string

	ConfigRepoDir  string
	ConfigFilename string

	OutputLocalStrategy string

	DepsLogFile  string
	FrontendFile string

	Fsopt              *hashfs.Option
	Reopt              *reapi.Option
	ReCacheEnableRead  bool
	ReCacheEnableWrite bool

	StartDir   string
	CredHelper string

	Targets []string
}

// Run runs the `ninja` subcommand.
func (c *NinjaOpts) Run(ctx context.Context) int {
	c.started = time.Now()

	if c.FrontendFile != "" {
		f := os.Stdout
		if c.FrontendFile != "-" {
			f, err = os.OpenFile(c.frontendFile, os.O_WRONLY|os.O_APPEND, 0644)
			if err != nil {
				ui.Default.Errorf("failed to open frontend file: %v\n", err)
				return 1
			}
			defer func() {
				err = f.Close()
				if err != nil {
					ui.Default.Errorf("failed to close frontend file: %v\n", err)
				}
			}()
		}
		frontend := soongutil.NewFrontend(ctx, f)
		ui.Default = frontend
		defer frontend.Close()
	}

	stats, err := c.run(ctx)
	d := time.Since(c.started)
	sps := float64(stats.Done-stats.Skipped) / d.Seconds()
	dur := ui.FormatDuration(d)
	if err != nil {
		var errFlag flagError
		var errBuild buildError
		switch {
		case errors.Is(err, errNothingToDo):
			msgPrefix := "Everything is up-to-date"
			msgPrefix = ui.SGR(ui.Green, msgPrefix)
			fmt.Fprintf(os.Stderr, "%s Nothing to do.\n", msgPrefix)
			return 0

		case errors.As(err, &errFlag):
			ui.Default.Errorf("%v\n", err)

		case errors.As(err, &errBuild):
			var errTarget build.TargetError
			if errors.As(errBuild.err, &errTarget) {
				msgPrefix := "Schedule Failure"
				dur = ui.SGR(ui.Bold, dur)
				msgPrefix = ui.SGR(ui.BackgroundRed, msgPrefix)
				fmt.Fprintf(os.Stderr, "\n%6s %s: %v\n", dur, msgPrefix, errTarget)
				if len(errTarget.Suggests) > 0 {
					var sb strings.Builder
					fmt.Fprintf(&sb, "Did you mean:")
					for _, s := range errTarget.Suggests {
						fmt.Fprintf(&sb, " %q", s)
					}
					fmt.Fprintln(&sb, " ?")
					ui.Default.Warningf("%s\n", sb.String())
				}
				return 1
			}
			var errMissingSource build.MissingSourceError
			if errors.As(errBuild.err, &errMissingSource) {
				msgPrefix := "Schedule Failure"
				dur = ui.SGR(ui.Bold, dur)
				msgPrefix = ui.SGR(ui.BackgroundRed, msgPrefix)
				fmt.Fprintf(os.Stderr, "\n%6s %s: %v\n", dur, msgPrefix, errMissingSource)
				return 1
			}
			msgPrefix := "Build Failure"
			dur = ui.SGR(ui.Bold, dur)
			msgPrefix = ui.SGR(ui.BackgroundRed, msgPrefix)
			fmt.Fprintf(os.Stderr, "\n%6s %s: %d done %d remaining - %.02f/s\n %v\n", dur, msgPrefix, stats.Done-stats.Skipped, stats.Total-stats.Done, sps, errBuild.err)
		default:
			msgPrefix := "Error"
			msgPrefix = ui.SGR(ui.BackgroundRed, msgPrefix)
			if status.Code(err) == codes.Unavailable {
				ui.Default.Errorf("\n%6s %s: could not connect to backend. If you want to build offline, pass `-o` or `--offline`\n %v\n", ui.FormatDuration(time.Since(c.started)), msgPrefix, err)
			} else {
				ui.Default.Errorf("\n%6s %s: %v\n", ui.FormatDuration(time.Since(c.started)), msgPrefix, err)
			}
		}
		return 1
	}
	msgPrefix := "Build Succeeded"
	dur = ui.SGR(ui.Bold, dur)
	msgPrefix = ui.SGR(ui.Green, msgPrefix)
	fmt.Fprintf(os.Stderr, "%6s %s: %d steps - %.02f/s\n", dur, msgPrefix, stats.Done-stats.Skipped, sps)
	return 0
}

type buildError struct {
	err error
}

func (b buildError) Error() string {
	return b.err.Error()
}

type flagError struct {
	err error
}

func (f flagError) Error() string {
	return f.err.Error()
}

var errNothingToDo = errors.New("nothing to do")

func (c *NinjaOpts) run(ctx context.Context) (stats build.Stats, err error) {
	if c.Offline {
		fmt.Fprintln(os.Stderr, ui.SGR(ui.Red, "offline mode"))
		log.Warnf("offline mode")
		c.Reopt = new(reapi.Option)
		c.Reopt.Insecure = true
		c.ProjectID = ""
	}

	execRoot, err := c.initWorkdirs()
	if err != nil {
		return stats, err
	}

	buildPath := build.NewPath(execRoot, c.Dir)

	// compute default limits based on fstype of work dir, not of exec root.
	limits := build.DefaultLimits()
	if c.LocalJobs > 0 {
		limits.Local = c.LocalJobs
	}
	if c.RemoteJobs > 0 {
		limits.Remote = c.RemoteJobs
	}

	projectID := c.Reopt.UpdateProjectID(c.ProjectID)

	var sisoMetadata ninjalog.SisoMetadata

	var credential cred.Cred
	if !c.offline && (c.reopt.NeedCred()) {
		log.Infof("init credentials")
		credential, err = cred.New(ctx, c.Ts)
		if err != nil {
			return stats, err
		}
	}
	// logging is ready.
	log.Infof("%s", cpuinfo())
	log.Infof("%s", gcinfo())

	log.Infof("siso version %s", c.version)
	sisoMetadata.SisoVersion = c.version
	if cmdver, err := version.GetStartupVersion(); err != nil {
		log.Warnf("cannot determine CIPD package version: %s", err)
	} else if cmdver.PackageName != "" {
		log.Infof("CIPD package name: %s", cmdver.PackageName)
		log.Infof("CIPD instance ID: %s", cmdver.InstanceID)
	} else {
		buildInfo, ok := debug.ReadBuildInfo()
		if ok {
			if buildInfo.GoVersion != "" {
				log.Infof("Go version: %s", buildInfo.GoVersion)
			}
			log.Infof("module %s %s %s", buildInfo.Main.Path, buildInfo.Main.Version, buildInfo.Main.Sum)
			for _, s := range buildInfo.Settings {
				if strings.HasPrefix(s.Key, "vcs.") || strings.HasPrefix(s.Key, "-") {
					log.Infof("build_%s=%s", s.Key, s.Value)
				}
			}
		}
	}
	c.checkResourceLimits(limits)

	targets := c.Targets
	config, err := c.initConfig(ctx, execRoot, targets)
	if err != nil {
		return stats, err
	}

	var eg errgroup.Group
	var localDepsLog *ninjautil.DepsLog
	eg.Go(func() error {
		depsLog, err := c.initDepsLog()
		if err != nil {
			return err
		}
		localDepsLog = depsLog
		return nil
	})

	if c.Reopt.IsValid() {
		log.Infof("use %s", c.Reopt)
	}
	ds, err := c.initDataSource(ctx, credential)
	if err != nil {
		return stats, err
	}
	defer func() {
		err := ds.Close()
		if err != nil {
			log.Errorf("close datasource: %v", err)
		}
	}()
	c.Fsopt.DataSource = ds
	c.Fsopt.OutputLocal, err = c.initOutputLocal()
	if err != nil {
		return stats, err
	}
	cwd := filepath.Join(execRoot, c.Dir)

	// ignore siso files not to be captured by ReadDir
	// (i.g. scandeps for -I.)
	c.Fsopt.Ignore = func(ctx context.Context, fname string) bool {
		dir, base := filepath.Split(fname)
		// allow siso prefix in other dir.
		// e.g. siso.gni exists in build/config/siso.
		if filepath.Clean(dir) != cwd {
			return false
		}
		if strings.HasPrefix(base, ".siso_") {
			return true
		}
		if strings.HasPrefix(base, "siso.") {
			return true
		}
		if strings.HasPrefix(base, "siso_") {
			return true
		}
		return false
	}

	log.Infof("loading fs state")

	hashFS, err := hashfs.New(ctx, *c.Fsopt)
	if err != nil {
		return stats, err
	}
	defer func() {
		hashFS.SetBuildTargets(targets, !c.DryRun && err == nil)
		err := hashFS.Close(ctx)
		if err != nil {
			log.Errorf("close hashfs: %v", err)
		}
	}()
	hashFSErr := hashFS.LoadErr()
	if hashFSErr != nil {
		ui.Default.Errorf(ui.SGR(ui.BackgroundRed, fmt.Sprintf("unable to do incremental build as fs state is corrupted: %v\n", hashFSErr)))
	}

	isClean := hashFS.IsClean(targets)
	log.Infof("hashfs loaderr: %v clean: %t (%q)", hashFSErr, isClean, targets)

	bopts, err := c.initBuildOpts(projectID, buildPath, config, ds, hashFS, limits)
	if err != nil {
		return stats, err
	}
	log.Infof("loading/recompacting deps log")
	err = eg.Wait()
	if localDepsLog != nil {
		defer localDepsLog.Close()
	}
	// TODO(b/286501388): init concurrently for .siso_config/.siso_filegroups, build.ninja.
	log.Infof("load siso config")
	stepConfig, err := ninjabuild.NewStepConfig(ctx, config, buildPath, hashFS, c.Fname)
	if err != nil {
		return stats, err
	}

	log.Infof("load %s", c.Fname)
	nstate, err := ninjabuild.Load(ctx, c.Fname, buildPath)
	if err != nil {
		return stats, err
	}

	graph := ninjabuild.NewGraph(c.Fname, nstate, config, buildPath, hashFS, stepConfig, localDepsLog)

	j, err := json.Marshal(sisoMetadata)
	if err != nil {
		return stats, err
	}
	if err := os.WriteFile(sisoMetadataFilename, j, 0644); err != nil {
		return stats, err
	}

	return runNinja(ctx, c.Fname, graph, bopts, targets)
}

func runNinja(ctx context.Context, fname string, graph *ninjabuild.Graph, bopts build.Options, targets []string) (build.Stats, error) {
	for {
		log.Infof("build starts")
		stats, err := doBuild(ctx, graph, bopts, targets...)
		if errors.Is(err, build.ErrManifestModified) {
			if bopts.DryRun {
				return stats, nil
			}
			log.Infof("%s modified", fname)
			log.Infof("reloading")
			err := graph.Reload(ctx)
			if err != nil {
				return stats, err
			}
			log.Infof("reload done. build retry")
			continue
		}
		return stats, err
	}
}

func (c *NinjaOpts) initWorkdirs() (string, error) {
	// don't use $PWD for current directory
	// to avoid symlink issue. b/286779149
	pwd := os.Getenv("PWD")
	_ = os.Unsetenv("PWD") // no error for safe env key name.

	execRoot, err := os.Getwd()
	if pwd != "" {
		_ = os.Setenv("PWD", pwd) // no error to reset env with valid value.
	}
	if err != nil {
		return "", err
	}
	c.StartDir = execRoot
	log.Infof("wd: %s", execRoot)
	// The formatting of this string, complete with funny quotes, is
	// so Emacs can properly identify that the cwd has changed for
	// subsequent commands.
	// Don't print this if a tool is being used, so that tool output
	// can be piped into a file without this string showing up.
	if c.Dir != "." {
		log.Infof("ninja: Entering directory `%s'", c.Dir)
	}
	err = os.Chdir(c.Dir)
	if err != nil {
		return "", err
	}
	log.Infof("change dir to %s", c.Dir)
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	realCWD, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		log.Warnf("failed to eval symlinks %q: %v", cwd, err)
	} else if cwd != realCWD {
		log.Infof("cwd %s -> %s", cwd, realCWD)
		cwd = realCWD
	}
	if !filepath.IsAbs(c.ConfigRepoDir) {
		execRoot, err = build.DetectExecRoot(cwd, c.ConfigRepoDir)
		if err != nil {
			return "", err
		}
		c.ConfigRepoDir = filepath.Join(execRoot, c.ConfigRepoDir)
	}
	log.Infof("exec_root: %s", execRoot)

	// recalculate dir as relative to exec_root.
	// recipe may use absolute path for -C.
	rdir, err := filepath.Rel(execRoot, cwd)
	if err != nil {
		return "", err
	}
	if !filepath.IsLocal(rdir) {
		return "", fmt.Errorf("dir %q is out of exec root %q", cwd, execRoot)
	}
	c.Dir = rdir
	log.Infof("working_directory in exec_root: %s", c.Dir)
	if c.StartDir != execRoot {
		log.Infof("exec_root=%s dir=%s", execRoot, c.Dir)
	}
	_, err = os.Stat(c.Fname)
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("%s not found in %s. need `-C <dir>`?", c.Fname, cwd)
	}
	return execRoot, err
}

func (c *NinjaOpts) initFlags(targets []string) map[string]string {
	flags := make(map[string]string)
	flag.Visit(func(f *flag.Flag) {
		name := f.Name
		if name == "C" {
			name = "dir"
		}
		flags[name] = f.Value.String()
	})
	flags["project"] = c.projectID
	flags["batch"] = strconv.FormatBool(c.batch)
	flags["targets"] = strings.Join(targets, " ")
	return flags
}

func (c *NinjaOpts) initConfig(ctx context.Context, execRoot string, targets []string) (*buildconfig.Config, error) {
	if c.ConfigFilename == "" {
		return nil, errors.New("no config filename")
	}
	cfgrepos := map[string]fs.FS{
		"config":           os.DirFS(c.ConfigRepoDir),
		"config_overrides": os.DirFS(filepath.Join(execRoot, ".siso_remote")),
	}
	flags := c.initFlags(targets)
	config, err := buildconfig.New(ctx, c.ConfigFilename, flags, cfgrepos)
	if err != nil {
		return nil, err
	}
	if gnArgs, err := os.ReadFile("args.gn"); err == nil {
		err := config.Metadata.Set("args.gn", string(gnArgs))
		if err != nil {
			return nil, err
		}
	} else if errors.Is(err, fs.ErrNotExist) {
		log.Warnf("no args.gn: %v", err)
	} else {
		return nil, err
	}
	return config, nil
}

func (c *NinjaOpts) initDepsLog() (*ninjautil.DepsLog, error) {
	err := os.MkdirAll(filepath.Dir(c.DepsLogFile), 0755)
	if err != nil {
		log.Warnf("failed to mkdir for deps log: %v", err)
		return nil, err
	}
	depsLog, err := ninjautil.NewDepsLog(c.DepsLogFile)
	if err != nil {
		log.Warnf("failed to load deps log: %v", err)
		return nil, err
	}
	if !depsLog.NeedsRecompact() {
		return depsLog, nil
	}
	err = depsLog.Recompact()
	if err != nil {
		log.Warnf("failed to recompact deps log: %v", err)
		return nil, err
	}
	return depsLog, nil
}

func (c *NinjaOpts) initBuildOpts(projectID string, buildPath *build.Path, config *buildconfig.Config, ds dataSource, hashFS *hashfs.HashFS, limits build.Limits) (bopts build.Options, err error) {
	var actionSaltBytes []byte
	if c.ActionSalt != "" {
		actionSaltBytes = []byte(c.ActionSalt)
	}

	cache, err := build.NewCache(build.CacheOptions{
		Store: ds.cache,
	})
	if err != nil {
		log.Warnf("no cache enabled: %v", err)
	}
	bopts = build.Options{
		StartTime:          c.started,
		ProjectID:          projectID,
		Metadata:           config.Metadata,
		Path:               buildPath,
		HashFS:             hashFS,
		REAPIClient:        ds.client,
		RECacheEnableRead:  c.ReCacheEnableRead,
		RECacheEnableWrite: c.ReCacheEnableWrite,
		ActionSalt:         actionSaltBytes,
		OutputLocal:        build.OutputLocalFunc(c.Fsopt.OutputLocal),
		Cache:              cache,
		Clobber:            c.Clobber,
		DryRun:             c.DryRun,
		Limits:             limits,
	}
	return bopts, nil
}

func rebuildManifest(ctx context.Context, graph *ninjabuild.Graph, bopts build.Options) error {
	_, err := graph.Targets(ctx, graph.Filename())
	if err != nil {
		log.Warnf("don't rebuild manifest: no target for %s: %v", graph.Filename(), err)
		return nil
	}
	log.Infof("rebuild manifest")
	mfbopts := bopts
	mfbopts.Clobber = false
	mfbopts.RebuildManifest = graph.Filename()
	mfb, err := build.New(ctx, graph, mfbopts)
	if err != nil {
		return err
	}

	err = mfb.Build(ctx, "rebuild manifest", graph.Filename())
	cerr := mfb.Close()
	if cerr != nil {
		return fmt.Errorf("failed to close builder: %w", cerr)
	}
	return err
}

func doBuild(ctx context.Context, graph *ninjabuild.Graph, bopts build.Options, args ...string) (stats build.Stats, err error) {
	err = rebuildManifest(ctx, graph, bopts)
	if err != nil {
		return stats, err
	}

	b, err := build.New(ctx, graph, bopts)
	if err != nil {
		return stats, err
	}

	defer func(ctx context.Context) {
		cerr := b.Close()
		if cerr != nil {
			log.Warnf("failed to close builder: %v", cerr)
		}
	}(ctx)
	err = b.Build(ctx, "build", args...)

	if err != nil {
		if errors.As(err, &build.MissingSourceError{}) {
			return stats, err
		}
		if errors.As(err, &build.DependencyCycleError{}) {
			return stats, err
		}
	}

	stats = b.Stats()
	log.Infof("stats=%#v", stats)
	if err != nil {
		return stats, buildError{err: err}
	}
	if bopts.REAPIClient == nil {
		return stats, err
	}
	// TODO(b/266518906): wait for completion of uploading manifest
	return stats, err
}

type dataSource struct {
	cache  cachestore.CacheStore
	client *reapi.Client
}

func (c *NinjaOpts) initDataSource(ctx context.Context, credential cred.Cred) (dataSource, error) {
	layeredCache := build.NewLayeredCache()
	var ds dataSource
	var err error
	if c.Reopt.IsValid() {
		ds.client, err = reapi.New(ctx, credential, *c.Reopt)
		if err != nil {
			return ds, err
		}
		layeredCache.AddLayer(ds.client.CacheStore())
	}
	ds.cache = layeredCache
	return ds, nil
}

func (ds dataSource) Close() error {
	if ds.client == nil {
		return nil
	}
	return ds.client.Close()
}

func (ds dataSource) DigestData(ctx context.Context, d digest.Digest, fname string) digest.Data {
	return digest.NewData(ds.Source(ctx, d, fname), d)
}

func (ds dataSource) Source(_ context.Context, d digest.Digest, fname string) digest.Source {
	return source{
		dataSource: ds,
		d:          d,
		fname:      fname,
	}
}

type source struct {
	dataSource dataSource
	d          digest.Digest
	fname      string
}

func (s source) Open(ctx context.Context) (io.ReadCloser, error) {
	if s.dataSource.cache != nil {
		src := s.dataSource.cache.Source(ctx, s.d, s.fname)
		if src != nil {
			r, err := src.Open(ctx)
			if err == nil {
				return r, nil
			}
		}
		// fallback
	}
	if s.dataSource.client != nil {
		buf, err := s.dataSource.client.Get(ctx, s.d, s.fname)
		if err == nil {
			return io.NopCloser(bytes.NewReader(buf)), nil
		}
		// fallback
	}
	// no reapi configured. use local file?
	f, err := os.Open(s.fname)
	return f, err
}

func (s source) String() string {
	return fmt.Sprintf("dataSource:%s", s.fname)
}

func (c *NinjaOpts) initOutputLocal() (func(context.Context, string) bool, error) {
	switch c.OutputLocalStrategy {
	case "full":
		return func(context.Context, string) bool { return true }, nil
	case "greedy":
		return func(ctx context.Context, fname string) bool {
			// Note: d. wil be downloaded to get deps anyway,
			// but will not be written to disk.
			switch filepath.Ext(fname) {
			case ".o", ".obj", ".a", ".d", ".stamp":
				return false
			}
			return true
		}, nil
	case "minimum":
		return func(ctx context.Context, fname string) bool {
			// force to output local for inputs
			// .h,/.hxx/.hpp/.inc/.c/.cc/.cxx/.cpp/.m/.mm for gcc deps
			// .json/.js/.ts for tsconfig.json, .js for grit etc.
			// .py for protobuf py etc.
			switch filepath.Ext(fname) {
			case ".h", ".hxx", ".hpp", ".inc", ".c", ".cc", "cxx", ".cpp", ".m", ".mm", ".json", ".js", ".ts", ".py":
				return true
			}
			return false
		}, nil
	default:
		return nil, fmt.Errorf("unknown output local strategy: %q. should be full/greedy/minimum", c.OutputLocalStrategy)
	}
}

func cpuinfo() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "cpu family=%d model=%d stepping=%d ", cpuid.CPU.Family, cpuid.CPU.Model, cpuid.CPU.Stepping)
	fmt.Fprintf(&sb, "brand=%q vendor=%q ", cpuid.CPU.BrandName, cpuid.CPU.VendorString)
	fmt.Fprintf(&sb, "physicalCores=%d threadsPerCore=%d logicalCores=%d ", cpuid.CPU.PhysicalCores, cpuid.CPU.ThreadsPerCore, cpuid.CPU.LogicalCores)
	fmt.Fprintf(&sb, "vm=%t features=%s", cpuid.CPU.VM(), cpuid.CPU.FeatureSet())
	return sb.String()
}

func gcinfo() string {
	var sb strings.Builder
	memoryLimit := debug.SetMemoryLimit(-1) // not adjust the limit, but retrieve current limit
	if memoryLimit == math.MaxInt64 {
		// initial settings
		fmt.Fprintf(&sb, "memory_limit=unlimited ")
	} else {
		fmt.Fprintf(&sb, "memory_limit=%d (GOMEMLIMIT=%s) ", memoryLimit, os.Getenv("GOMEMLIMIT"))
	}

	gcPercent := debug.SetGCPercent(100) // 100 is default
	if gcPercent < 0 {
		log.Warnf("Garbage collection is disabled. GOGC=%s\n", os.Getenv("GOGC"))
		fmt.Fprintf(&sb, "gc=off")
	} else {
		fmt.Fprintf(&sb, "gc=%d", gcPercent)
	}
	debug.SetGCPercent(gcPercent) // restore original setting
	if v := os.Getenv("GOGC"); v != "" {
		fmt.Fprintf(&sb, " (GOGC=%s)", v)
	}
	return sb.String()
}
