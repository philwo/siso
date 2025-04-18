// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package ninja implements the subcommand `ninja` which parses a `build.ninja` file and builds the requested targets.
package ninja

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/log"
	"github.com/klauspost/cpuid/v2"
	"github.com/maruel/subcommands"
	"golang.org/x/oauth2"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"go.chromium.org/luci/cipd/version"
	"go.chromium.org/luci/common/cli"

	"go.chromium.org/infra/build/siso/auth/cred"
	"go.chromium.org/infra/build/siso/build"
	"go.chromium.org/infra/build/siso/build/buildconfig"
	"go.chromium.org/infra/build/siso/build/cachestore"
	"go.chromium.org/infra/build/siso/build/ninjabuild"
	"go.chromium.org/infra/build/siso/hashfs"
	"go.chromium.org/infra/build/siso/reapi"
	"go.chromium.org/infra/build/siso/reapi/digest"
	"go.chromium.org/infra/build/siso/toolsupport/ninjautil"
	"go.chromium.org/infra/build/siso/ui"
)

const ninjaUsage = `build the requested targets as ninja.

 $ siso ninja [-C <dir>] [options] [targets...]

`

// Cmd returns the Command for the `ninja` subcommand provided by this package.
func Cmd(ts oauth2.TokenSource, version string) *subcommands.Command {
	return &subcommands.Command{
		UsageLine: "ninja <args>...",
		ShortDesc: "build the requests targets as ninja",
		LongDesc:  ninjaUsage,
		CommandRun: func() subcommands.CommandRun {
			r := ninjaCmdRun{
				ts:      ts,
				version: version,
			}
			r.init()
			return &r
		},
	}
}

type ninjaCmdRun struct {
	subcommands.CommandRunBase
	ts      oauth2.TokenSource
	version string
	started time.Time

	// flag values
	dir        string
	configName string
	projectID  string

	offline    bool
	dryRun     bool
	clobber    bool
	actionSalt string

	remoteJobs int
	fname      string

	configRepoDir  string
	configFilename string

	outputLocalStrategy string

	depsLogFile string

	fsopt             *hashfs.Option
	reopt             *reapi.Option
	reCacheEnableRead bool

	startDir string
}

// Run runs the `ninja` subcommand.
func (c *ninjaCmdRun) Run(a subcommands.Application, args []string, env subcommands.Env) int {
	c.started = time.Now()
	ctx := cli.GetContext(a, c, env)
	err := parseFlagsFully(&c.Flags)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
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
			fmt.Fprintf(os.Stderr, "%v\n", err)

		case errors.As(err, &errBuild):
			var errTarget build.TargetError
			if errors.As(errBuild.err, &errTarget) {
				msgPrefix := "Schedule Failure"
				dur = ui.SGR(ui.Bold, dur)
				msgPrefix = ui.SGR(ui.BackgroundRed, msgPrefix)
				fmt.Fprintf(os.Stderr, "\n%6s %s: %v\n", dur, msgPrefix, errTarget)
				if len(errTarget.Suggests) > 0 {
					fmt.Fprintf(os.Stderr, "Did you mean:")
					for _, s := range errTarget.Suggests {
						fmt.Fprintf(os.Stderr, " %q", s)
					}
					fmt.Fprintln(os.Stderr, " ?")
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
				fmt.Fprintf(os.Stderr, "\n%6s %s: could not connect to backend. If you want to build offline, pass `-o` or `--offline`\n %v\n", ui.FormatDuration(time.Since(c.started)), msgPrefix, err)
			} else {
				fmt.Fprintf(os.Stderr, "\n%6s %s: %v\n", ui.FormatDuration(time.Since(c.started)), msgPrefix, err)
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

// parse flags without stopping at non flags.
func parseFlagsFully(flagSet *flag.FlagSet) error {
	var targets []string
	for {
		args := flagSet.Args()
		if len(args) == 0 {
			break
		}
		argsRemaining := len(args)
		for i, arg := range args {
			if !strings.HasPrefix(arg, "-") {
				targets = append(targets, arg)
				argsRemaining--
				continue
			}
			err := flagSet.Parse(args[i:])
			if err != nil {
				return err
			}
			break
		}
		if argsRemaining == 0 {
			break
		}
	}
	// targets are non-flags. set it to Args.
	return flagSet.Parse(targets)
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

type errInterrupted struct{}

func (errInterrupted) Error() string        { return "interrupt by signal" }
func (errInterrupted) Is(target error) bool { return target == context.Canceled }

// HandleInterrupt calls 'fn' in a separate goroutine on SIGTERM or Ctrl+C.
//
// When SIGTERM or Ctrl+C comes for a second time, logs to stderr and kills
// the process immediately via os.Exit(1).
//
// Returns a callback that can be used to remove the installed signal handlers.
func HandleInterrupt(fn func()) (stopper func()) {
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)

	go func() {
		handled := false
		for range ch {
			if handled {
				fmt.Fprintf(os.Stderr, "Got second interrupt signal. Aborting.\n")
				os.Exit(1)
			}
			handled = true
			go fn()
		}
	}()

	return func() {
		signal.Stop(ch)
		close(ch)
	}
}

func (c *ninjaCmdRun) run(ctx context.Context) (stats build.Stats, err error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer HandleInterrupt(func() {
		cancel(errInterrupted{})
	})()

	if c.offline {
		fmt.Fprintln(os.Stderr, ui.SGR(ui.Red, "offline mode"))
		log.Warnf("offline mode")
		c.reopt = new(reapi.Option)
		c.projectID = ""
	}

	execRoot, err := c.initWorkdirs()
	if err != nil {
		return stats, err
	}

	buildPath := build.NewPath(execRoot, c.dir)

	// compute default limits based on fstype of work dir, not of exec root.
	limits := build.DefaultLimits()
	if c.remoteJobs > 0 {
		limits.Remote = c.remoteJobs
	}

	projectID := c.reopt.UpdateProjectID(c.projectID)

	var credential cred.Cred
	if projectID != "" {
		log.Infof("init credentials")
		credential, err = cred.New(ctx, c.ts)
		if err != nil {
			return stats, err
		}
	}
	// logging is ready.
	log.Infof("%s", cpuinfo())
	log.Infof("%s", gcinfo())

	log.Infof("siso version %s", c.version)
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
			for _, s := range buildInfo.Settings {
				if strings.HasPrefix(s.Key, "vcs.") || strings.HasPrefix(s.Key, "-") {
					log.Infof("build_%s=%s", s.Key, s.Value)
				}
			}
		}
	}
	c.checkResourceLimits()

	targets := c.Flags.Args()
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

	if c.reopt.IsValid() {
		log.Infof("reapi instance: %s", c.reopt.Instance)
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
	c.fsopt.DataSource = ds
	c.fsopt.OutputLocal, err = c.initOutputLocal()
	if err != nil {
		return stats, err
	}
	cwd := filepath.Join(execRoot, c.dir)

	// ignore siso files not to be captured by ReadDir
	// (i.g. scandeps for -I.)
	c.fsopt.Ignore = func(ctx context.Context, fname string) bool {
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

	hashFS, err := hashfs.New(ctx, *c.fsopt)
	if err != nil {
		return stats, err
	}
	defer func() {
		hashFS.SetBuildTargets(targets, !c.dryRun && err == nil)
		err := hashFS.Close(ctx)
		if err != nil {
			log.Errorf("close hashfs: %v", err)
		}
	}()
	hashFSErr := hashFS.LoadErr()
	if hashFSErr != nil {
		fmt.Fprintln(os.Stderr, ui.SGR(ui.BackgroundRed, fmt.Sprintf("unable to do incremental build as fs state is corrupted: %v", hashFSErr)))
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
	stepConfig, err := ninjabuild.NewStepConfig(ctx, config, buildPath, hashFS, c.fname)
	if err != nil {
		return stats, err
	}

	log.Infof("load %s", c.fname)
	nstate, err := ninjabuild.Load(ctx, c.fname, buildPath)
	if err != nil {
		return stats, err
	}

	graph := ninjabuild.NewGraph(c.fname, nstate, config, buildPath, hashFS, stepConfig, localDepsLog)

	return runNinja(ctx, c.fname, graph, bopts, targets)
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

func (c *ninjaCmdRun) init() {
	c.Flags.StringVar(&c.dir, "C", ".", "ninja running directory")
	c.Flags.StringVar(&c.configName, "config", "", "config name passed to starlark")
	c.Flags.StringVar(&c.projectID, "project", os.Getenv("SISO_PROJECT"), "cloud project ID. can set by $SISO_PROJECT")

	c.Flags.BoolVar(&c.offline, "offline", false, "offline mode.")
	c.Flags.BoolVar(&c.offline, "o", false, "alias of `-offline`")
	if f := c.Flags.Lookup("offline"); f != nil {
		if s := os.Getenv("RBE_remote_disabled"); s != "" {
			err := f.Value.Set(s)
			if err != nil {
				log.Errorf("invalid RBE_remote_disabled=%q: %v", s, err)
			}
		}
	}
	c.Flags.BoolVar(&c.dryRun, "n", false, "dry run")
	c.Flags.BoolVar(&c.clobber, "clobber", false, "clobber build")
	c.Flags.StringVar(&c.actionSalt, "action_salt", "", "action salt")

	c.Flags.IntVar(&c.remoteJobs, "remote_jobs", 0, "run N remote jobs in parallel. when the value is <= 0, it will be computed based on # of CPUs.")
	c.Flags.StringVar(&c.fname, "f", "build.ninja", "input build manifest filename (relative to -C)")

	c.Flags.StringVar(&c.configRepoDir, "config_repo_dir", "build/config/siso", "config repo directory (relative to exec root)")
	c.Flags.StringVar(&c.configFilename, "load", "@config//main.star", "config filename (@config// is --config_repo_dir)")
	c.Flags.StringVar(&c.outputLocalStrategy, "output_local_strategy", "full", `strategy for output_local. "full": download all outputs. "greedy": downloads most outputs except intermediate objs. "minimum": downloads as few as possible`)
	c.Flags.StringVar(&c.depsLogFile, "deps_log", ".siso_deps", "deps log filename (relative to -C)")

	c.fsopt = new(hashfs.Option)
	c.fsopt.StateFile = ".siso_fs_state"
	c.fsopt.RegisterFlags(&c.Flags)

	c.reopt = new(reapi.Option)
	envs := map[string]string{
		"SISO_REAPI_INSTANCE": os.Getenv("SISO_REAPI_INSTANCE"),
		"SISO_REAPI_ADDRESS":  os.Getenv("SISO_REAPI_ADDRESS"),
	}
	c.reopt.RegisterFlags(&c.Flags, envs)
	c.Flags.BoolVar(&c.reCacheEnableRead, "re_cache_enable_read", true, "remote exec cache enable read")
}

func (c *ninjaCmdRun) initWorkdirs() (string, error) {
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
	c.startDir = execRoot
	log.Infof("wd: %s", execRoot)
	// The formatting of this string, complete with funny quotes, is
	// so Emacs can properly identify that the cwd has changed for
	// subsequent commands.
	// Don't print this if a tool is being used, so that tool output
	// can be piped into a file without this string showing up.
	if c.dir != "." {
		log.Infof("ninja: Entering directory `%s'", c.dir)
	}
	err = os.Chdir(c.dir)
	if err != nil {
		return "", err
	}
	log.Infof("change dir to %s", c.dir)
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
	if !filepath.IsAbs(c.configRepoDir) {
		execRoot, err = build.DetectExecRoot(cwd, c.configRepoDir)
		if err != nil {
			return "", err
		}
		c.configRepoDir = filepath.Join(execRoot, c.configRepoDir)
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
	c.dir = rdir
	log.Infof("working_directory in exec_root: %s", c.dir)
	if c.startDir != execRoot {
		log.Infof("exec_root=%s dir=%s", execRoot, c.dir)
	}
	_, err = os.Stat(c.fname)
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("%s not found in %s. need `-C <dir>`?", c.fname, cwd)
	}
	return execRoot, err
}

func (c *ninjaCmdRun) initFlags(targets []string) map[string]string {
	flags := make(map[string]string)
	c.Flags.Visit(func(f *flag.Flag) {
		name := f.Name
		if name == "C" {
			name = "dir"
		}
		flags[name] = f.Value.String()
	})
	flags["targets"] = strings.Join(targets, " ")
	return flags
}

func (c *ninjaCmdRun) initConfig(ctx context.Context, execRoot string, targets []string) (*buildconfig.Config, error) {
	if c.configFilename == "" {
		return nil, errors.New("no config filename")
	}
	cfgrepos := map[string]fs.FS{
		"config":           os.DirFS(c.configRepoDir),
		"config_overrides": os.DirFS(filepath.Join(execRoot, ".siso_remote")),
	}
	flags := c.initFlags(targets)
	config, err := buildconfig.New(ctx, c.configFilename, flags, cfgrepos)
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

func (c *ninjaCmdRun) initDepsLog() (*ninjautil.DepsLog, error) {
	err := os.MkdirAll(filepath.Dir(c.depsLogFile), 0755)
	if err != nil {
		log.Warnf("failed to mkdir for deps log: %v", err)
		return nil, err
	}
	depsLog, err := ninjautil.NewDepsLog(c.depsLogFile)
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

func (c *ninjaCmdRun) initBuildOpts(projectID string, buildPath *build.Path, config *buildconfig.Config, ds dataSource, hashFS *hashfs.HashFS, limits build.Limits) (bopts build.Options, err error) {
	var actionSaltBytes []byte
	if c.actionSalt != "" {
		actionSaltBytes = []byte(c.actionSalt)
	}

	cache, err := build.NewCache(build.CacheOptions{
		Store: ds.cache,
	})
	if err != nil {
		log.Warnf("no cache enabled: %v", err)
	}
	bopts = build.Options{
		StartTime:         c.started,
		ProjectID:         projectID,
		Metadata:          config.Metadata,
		Path:              buildPath,
		HashFS:            hashFS,
		REAPIClient:       ds.client,
		RECacheEnableRead: c.reCacheEnableRead,
		ActionSalt:        actionSaltBytes,
		OutputLocal:       build.OutputLocalFunc(c.fsopt.OutputLocal),
		Cache:             cache,
		Clobber:           c.clobber,
		DryRun:            c.dryRun,
		Limits:            limits,
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

func (c *ninjaCmdRun) initDataSource(ctx context.Context, credential cred.Cred) (dataSource, error) {
	layeredCache := build.NewLayeredCache()
	var ds dataSource
	var err error
	if c.reopt.IsValid() {
		ds.client, err = reapi.New(ctx, credential, *c.reopt)
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

func (c *ninjaCmdRun) initOutputLocal() (func(context.Context, string) bool, error) {
	switch c.outputLocalStrategy {
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
		return nil, fmt.Errorf("unknown output local strategy: %q. should be full/greedy/minimum", c.outputLocalStrategy)
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
