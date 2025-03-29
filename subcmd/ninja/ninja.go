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
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/log"
	"github.com/klauspost/cpuid/v2"
	"github.com/maruel/subcommands"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"go.chromium.org/luci/auth"
	"go.chromium.org/luci/cipd/version"
	"go.chromium.org/luci/common/cli"
	"go.chromium.org/luci/common/system/signals"

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
func Cmd(authOpts cred.Options, version string) *subcommands.Command {
	return &subcommands.Command{
		UsageLine: "ninja <args>...",
		ShortDesc: "build the requests targets as ninja",
		LongDesc:  ninjaUsage,
		CommandRun: func() subcommands.CommandRun {
			r := ninjaCmdRun{
				authOpts: authOpts,
				version:  version,
			}
			r.init()
			return &r
		},
	}
}

type ninjaCmdRun struct {
	subcommands.CommandRunBase
	authOpts cred.Options
	version  string
	started  time.Time

	// flag values
	dir        string
	configName string
	projectID  string

	offline         bool
	batch           bool
	verbose         bool
	verboseFailures bool
	dryRun          bool
	clobber         bool
	prepare         bool
	failuresAllowed int
	actionSalt      string

	ninjaJobs      int
	ninjaLoadLimit int

	remoteJobs int
	fname      string

	configRepoDir  string
	configFilename string

	outputLocalStrategy string

	depsLogFile string
	// depsLogBucket

	logDir             string
	failureSummaryFile string

	fsopt             *hashfs.Option
	reopt             *reapi.Option
	reCacheEnableRead bool
	// reCacheEnableWrite bool

	// enableCPUProfiler bool

	subtool    string
	cleandead  bool
	debugMode  debugMode
	adjustWarn string

	sisoInfoLog string // abs or relative to logDir
	startDir    string
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
		case errors.Is(err, auth.ErrLoginRequired):
			fmt.Fprintf(os.Stderr, "need to login: run `siso login`\n")
		case errors.Is(err, errNothingToDo):
			msgPrefix := "Everything is up-to-date"
			if ui.IsTerminal() {
				msgPrefix = ui.SGR(ui.Green, msgPrefix)
			}
			fmt.Fprintf(os.Stderr, "%s Nothing to do.\n", msgPrefix)
			return 0

		case errors.As(err, &errFlag):
			fmt.Fprintf(os.Stderr, "%v\n", err)

		case errors.As(err, &errBuild):
			var errTarget build.TargetError
			if errors.As(errBuild.err, &errTarget) {
				msgPrefix := "Schedule Failure"
				if ui.IsTerminal() {
					dur = ui.SGR(ui.Bold, dur)
					msgPrefix = ui.SGR(ui.BackgroundRed, msgPrefix)
				}
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
				if ui.IsTerminal() {
					dur = ui.SGR(ui.Bold, dur)
					msgPrefix = ui.SGR(ui.BackgroundRed, msgPrefix)
				}
				fmt.Fprintf(os.Stderr, "\n%6s %s: %v\n", dur, msgPrefix, errMissingSource)
				return 1
			}
			msgPrefix := "Build Failure"
			if ui.IsTerminal() {
				dur = ui.SGR(ui.Bold, dur)
				msgPrefix = ui.SGR(ui.BackgroundRed, msgPrefix)
			}
			fmt.Fprintf(os.Stderr, "\n%6s %s: %d done %d remaining - %.02f/s\n %v\n", dur, msgPrefix, stats.Done-stats.Skipped, stats.Total-stats.Done, sps, errBuild.err)
		default:
			msgPrefix := "Error"
			if ui.IsTerminal() {
				msgPrefix = ui.SGR(ui.BackgroundRed, msgPrefix)
			}
			if status.Code(err) == codes.Unavailable {
				fmt.Fprintf(os.Stderr, "\n%6s %s: could not connect to backend. If you want to build offline, pass `-o` or `--offline`\n %v\n", ui.FormatDuration(time.Since(c.started)), msgPrefix, err)
			} else {
				fmt.Fprintf(os.Stderr, "\n%6s %s: %v\n", ui.FormatDuration(time.Since(c.started)), msgPrefix, err)
			}
		}
		return 1
	}
	msgPrefix := "Build Succeeded"
	if ui.IsTerminal() {
		dur = ui.SGR(ui.Bold, dur)
		msgPrefix = ui.SGR(ui.Green, msgPrefix)
	}
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

type errAlreadyLocked struct {
	err     error
	bufErr  error
	fname   string
	pidfile string
	owner   string
}

func (l errAlreadyLocked) Error() string {
	if l.bufErr != nil && l.pidfile != "" {
		return fmt.Sprintf("%s is locked, and failed to read %s: %v", l.fname, l.pidfile, l.bufErr)
	} else if l.bufErr != nil {
		return fmt.Sprintf("%s is locked, and failed to read: %v", l.fname, l.bufErr)
	}
	return fmt.Sprintf("%s is locked by %s: %v", l.fname, l.owner, l.err)
}
func (l errAlreadyLocked) Unwrap() error {
	if l.err != nil {
		return l.err
	}
	return l.bufErr
}

type errInterrupted struct{}

func (errInterrupted) Error() string        { return "interrupt by signal" }
func (errInterrupted) Is(target error) bool { return target == context.Canceled }

const (
	// relative to -log_dir
	failedTargetsFile = ".siso_failed_targets"
)

func (c *ninjaCmdRun) run(ctx context.Context) (stats build.Stats, err error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer signals.HandleInterrupt(func() {
		cancel(errInterrupted{})
	})()
	err = c.debugMode.check()
	if err != nil {
		return stats, flagError{err: err}
	}
	switch c.subtool {
	case "":
	case "list":
		return stats, flagError{
			err: errors.New(`ninja subtools:
  commands   Use "siso query commands" instead
  deps       Use "siso query deps" instead
  inputs     Use "siso query inputs" instead
  targets    Use "siso query targets" instead
  cleandead  clean built files that are no longer produced by the manifest`),
		}
	case "commands":
		return stats, flagError{
			err: errors.New("use `siso query commands` instead"),
		}
	case "deps":
		return stats, flagError{
			err: errors.New("use `siso query deps` instead"),
		}
	case "inputs":
		return stats, flagError{
			err: errors.New("use `siso query inputs` instead"),
		}
	case "targets":
		return stats, flagError{
			err: errors.New("use `siso query targets` instead"),
		}

	case "cleandead":
		c.cleandead = true
	default:
		return stats, flagError{err: fmt.Errorf("unknown tool %q", c.subtool)}
	}

	if c.ninjaJobs >= 0 {
		fmt.Fprintf(os.Stderr, "-j is not supported. use -remote_jobs instead\n")
	}
	if c.ninjaLoadLimit >= 0 {
		fmt.Fprintf(os.Stderr, "-l is not supported.\n")
	}
	if c.failuresAllowed <= 0 {
		c.failuresAllowed = math.MaxInt
	}
	if c.failuresAllowed > 1 {
		c.batch = true
	}

	if c.adjustWarn != "" {
		fmt.Fprintf(os.Stderr, "-w is specified. but not supported. b/288807840\n")
	}

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
	if !c.dryRun {
		lock, err := newLockFile(".siso_lock")
		switch {
		case errors.Is(err, errors.ErrUnsupported):
			log.Warnf("lockfile is not supported")
		case err != nil:
			return stats, err
		default:
			var owner string
			spin := ui.Default.NewSpinner()
			for {
				err = lock.Lock()
				alreadyLocked := &errAlreadyLocked{}
				if errors.As(err, &alreadyLocked) {
					if owner != alreadyLocked.owner {
						if owner != "" {
							spin.Done("lock holder %s completed", owner)
						}
						owner = alreadyLocked.owner
						spin.Start("waiting for lock holder %s..", owner)
					}
					select {
					case <-ctx.Done():
						return stats, context.Cause(ctx)
					case <-time.After(500 * time.Millisecond):
						continue
					}
				} else if err != nil {
					spin.Stop(err)
					return stats, err
				}
				if owner != "" {
					spin.Done("lock holder %s completed", owner)
				}
				break
			}
			defer func() {
				err := lock.Unlock()
				if err != nil {
					fmt.Fprintf(os.Stderr, "failed to unlock .siso_lock: %v\n", err)
				}
				err = lock.Close()
				if err != nil {
					fmt.Fprintf(os.Stderr, "failed to close .siso_lock: %v\n", err)
				}
			}()
		}
	}

	isLogDirDefault := c.logDir == "."
	err = c.initLogDir()
	if err != nil {
		return stats, err
	}

	resetCrashOutput, err := c.setupCrashOutput()
	if err != nil {
		return stats, err
	}
	defer resetCrashOutput()

	buildPath := build.NewPath(execRoot, c.dir)

	// compute default limits based on fstype of work dir, not of exec root.
	limits := build.DefaultLimits()
	if c.remoteJobs > 0 {
		limits.Remote = c.remoteJobs
	}

	projectID := c.reopt.UpdateProjectID(c.projectID)

	var credential cred.Cred
	if projectID != "" {
		// TODO: can be async until cred is needed?
		spin := ui.Default.NewSpinner()
		spin.Start("init credentials")
		credential, err = cred.New(ctx, c.authOpts)
		if err != nil {
			if !c.reopt.NeedCred() {
				log.Warnf("failed to init credential: %v", err)
				log.Warnf("but no remote apis require credential")
			} else {
				spin.Stop(errors.New(""))
				return stats, err
			}
		}
		spin.Stop(nil)
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

	log.Infof("project id: %q", projectID)
	log.Infof("commandline %q", os.Args)
	log.Infof("is_terminal=%t batch=%t", ui.IsTerminal(), c.batch)

	spin := ui.Default.NewSpinner()

	targets := c.Flags.Args()
	config, err := c.initConfig(ctx, execRoot, targets)
	if err != nil {
		return stats, err
	}

	failedTargetsFilename := c.logFilename(failedTargetsFile, "")

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
		ui.Default.PrintLines(fmt.Sprintf("reapi instance: %s", c.reopt.Instance))
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
	if c.logDir == "." || c.logDir == filepath.Join(execRoot, c.dir) {
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
	}

	spin.Start("loading fs state")

	hashFS, err := hashfs.New(ctx, *c.fsopt)
	spin.Stop(err)
	if err != nil {
		return stats, err
	}
	defer func() {
		if c.dryRun {
			return
		}
		if c.subtool != "" {
			// don't modify .siso_failed_targets, .siso_last_targets by subtool.
			return
		}
		if c.prepare {
			return
		}
		if err != nil {
			// when batch mode, no need to record failed targets,
			// as it will build full targets when rebuilding
			// for throughput, rather than latency.
			if c.batch {
				return
			}
			var errBuild buildError
			if !errors.As(err, &errBuild) {
				return
			}
			var stepError build.StepError
			if !errors.As(errBuild.err, &stepError) {
				return
			}
			// store failed targets only when build steps failed.
			// i.e., don't store with error like context canceled, etc.
			log.Infof("record failed targets: %q", stepError.Target)
			serr := saveTargets(failedTargetsFilename, targets, []string{stepError.Target})
			if serr != nil {
				log.Warnf("failed to save failed targets: %v", serr)
				return
			}
		}
	}()
	defer func() {
		hashFS.SetBuildTargets(targets, !c.dryRun && c.subtool == "" && !c.prepare && err == nil)
		err := hashFS.Close(ctx)
		if err != nil {
			log.Errorf("close hashfs: %v", err)
		}
	}()
	hashFSErr := hashFS.LoadErr()
	if hashFSErr != nil {
		fmt.Fprintln(os.Stderr, ui.SGR(ui.BackgroundRed, fmt.Sprintf("unable to do incremental build as fs state is corrupted: %v", hashFSErr)))
	}

	_, err = os.Stat(failedTargetsFilename)
	lastFailed := err == nil
	isClean := hashFS.IsClean(targets)
	log.Infof("hashfs loaderr: %v clean: %t (%q) last failed: %t", hashFSErr, isClean, targets, lastFailed)
	// if not using non-default log_dir, it would see different
	// .siso_last_targets, which won't match with .siso_fs_state.
	// in this case, don't shortcut noop build, but better to check
	// build graph again.
	if !c.clobber && !c.batch && !c.dryRun && c.subtool != "cleandead" && isLogDirDefault && hashFSErr == nil && isClean && !lastFailed {
		// TODO: better to check digest of .siso_fs_state?
		return stats, errNothingToDo
	}

	bopts, done, err := c.initBuildOpts(projectID, buildPath, config, ds, hashFS, limits)
	if err != nil {
		return stats, err
	}
	defer done(&err)
	spin.Start("loading/recompacting deps log")
	err = eg.Wait()
	spin.Stop(err)
	if localDepsLog != nil {
		defer localDepsLog.Close()
	}
	// TODO(b/286501388): init concurrently for .siso_config/.siso_filegroups, build.ninja.
	spin.Start("load siso config")
	stepConfig, err := ninjabuild.NewStepConfig(ctx, config, buildPath, hashFS, c.fname)
	if err != nil {
		spin.Stop(err)
		return stats, err
	}
	spin.Stop(nil)
	if c.fsopt.KeepTainted {
		tainted := hashFS.TaintedFiles()
		if len(tainted) == 0 {
			ui.Default.PrintLines(ui.SGR(ui.Yellow, "no tainted generated files"))
		} else if len(tainted) < 5 {
			ui.Default.PrintLines(ui.SGR(ui.Yellow, fmt.Sprintf("keep %d tainted files: %s", len(tainted), strings.Join(tainted, ", "))))
		} else {
			ui.Default.PrintLines(ui.SGR(ui.Yellow, fmt.Sprintf("keep %d tainted files: %s ... more", len(tainted), strings.Join(tainted, ", "))))
		}
	}

	spin.Start(fmt.Sprintf("load %s", c.fname))
	nstate, err := ninjabuild.Load(ctx, c.fname, buildPath)
	if err != nil {
		spin.Stop(errors.New(""))
		return stats, err
	}
	spin.Stop(nil)

	graph := ninjabuild.NewGraph(c.fname, nstate, config, buildPath, hashFS, stepConfig, localDepsLog)

	var lastFailedTargets []string
	if !c.batch && !c.clobber {
		lastFailedTargets, _ = checkTargets(failedTargetsFilename, targets)
	}
	err = os.Remove(failedTargetsFilename)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Warnf("failed to remove %s: %v", failedTargetsFilename, err)
	}
	return runNinja(ctx, c.fname, graph, bopts, targets, runNinjaOpts{
		checkFailedTargets: lastFailedTargets,
		cleandead:          c.cleandead,
		subtool:            c.subtool,
		enableStatusz:      true,
	})
}

type runNinjaOpts struct {
	// build the last failed targets first.
	checkFailedTargets []string

	// whether to perform cleandead or not.
	cleandead bool

	// subtool name.
	// if "cleandead", it returns after cleandead performed.
	subtool string

	// enable statusz (for `siso ps`)
	enableStatusz bool
}

func runNinja(ctx context.Context, fname string, graph *ninjabuild.Graph, bopts build.Options, targets []string, nopts runNinjaOpts) (build.Stats, error) {
	var stats build.Stats
	spin := ui.Default.NewSpinner()

	for {
		log.Infof("build starts")
		if len(nopts.checkFailedTargets) > 0 {
			failedTargets := nopts.checkFailedTargets
			ui.Default.PrintLines(fmt.Sprintf("Building last failed targets: %s...", failedTargets))
			var err error
			stats, err = doBuild(ctx, graph, bopts, nopts, failedTargets...)
			if errors.Is(err, build.ErrManifestModified) {
				if bopts.DryRun {
					return stats, nil
				}
				log.Infof("%s modified.", fname)
				spin.Start("reloading")
				err := graph.Reload(ctx)
				if err != nil {
					spin.Stop(err)
					return stats, err
				}
				spin.Stop(nil)
				log.Infof("reload done. build retry")
				continue
			}
			var errBuild buildError
			if errors.As(err, &errBuild) {
				var stepError build.StepError
				if errors.As(errBuild.err, &stepError) {
					// last failed is not fixed yet.
					return stats, err
				}
			}
			nopts.checkFailedTargets = nil
			if err != nil {
				ui.Default.PrintLines(fmt.Sprintf(" %s: %s: %v", ui.SGR(ui.Yellow, "err in last failed targets, rebuild again"), failedTargets, err))
			} else {
				ui.Default.PrintLines(fmt.Sprintf(" %s: %s", ui.SGR(ui.Green, "last failed targets fixed"), failedTargets))
			}
			err = graph.Reset(ctx)
			if err != nil {
				return stats, err
			}
		}
		stats, err := doBuild(ctx, graph, bopts, nopts, targets...)
		if errors.Is(err, build.ErrManifestModified) {
			if bopts.DryRun {
				return stats, nil
			}
			log.Infof("%s modified", fname)
			spin.Start("reloading")
			err := graph.Reload(ctx)
			if err != nil {
				spin.Stop(err)
				return stats, err
			}
			spin.Stop(nil)
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
	c.Flags.BoolVar(&c.batch, "batch", !ui.IsTerminal(), "batch mode. prefer thoughput over low latency for build failures.")
	c.Flags.BoolVar(&c.verbose, "verbose", false, "show all command lines while building")
	c.Flags.BoolVar(&c.verbose, "v", false, "show all command lines while building (alias of --verbose)")
	c.Flags.BoolVar(&c.verboseFailures, "verbose_failures", true, "show failed command lines")
	c.Flags.BoolVar(&c.dryRun, "n", false, "dry run")
	c.Flags.BoolVar(&c.clobber, "clobber", false, "clobber build")
	c.Flags.BoolVar(&c.prepare, "prepare", false, "build inputs of targets, but not build target itself.")
	c.Flags.IntVar(&c.failuresAllowed, "k", 1, "keep going until N jobs fail (0 means inifinity)")
	c.Flags.StringVar(&c.actionSalt, "action_salt", "", "action salt")

	c.Flags.IntVar(&c.ninjaJobs, "j", -1, "not supported. use -remote_jobs instead")
	c.Flags.IntVar(&c.ninjaLoadLimit, "l", -1, "not supported.")
	c.Flags.IntVar(&c.remoteJobs, "remote_jobs", 0, "run N remote jobs in parallel. when the value is no positive, the default will be computed based on # of CPUs.")
	c.Flags.StringVar(&c.fname, "f", "build.ninja", "input build manifest filename (relative to -C)")

	c.Flags.StringVar(&c.configRepoDir, "config_repo_dir", "build/config/siso", "config repo directory (relative to exec root)")
	c.Flags.StringVar(&c.configFilename, "load", "@config//main.star", "config filename (@config// is --config_repo_dir)")
	c.Flags.StringVar(&c.outputLocalStrategy, "output_local_strategy", "full", `strategy for output_local. "full": download all outputs. "greedy": downloads most outputs except intermediate objs. "minimum": downloads as few as possible`)
	c.Flags.StringVar(&c.depsLogFile, "deps_log", ".siso_deps", "deps log filename (relative to -C)")

	c.Flags.StringVar(&c.logDir, "log_dir", ".", "log directory (relative to -C")
	c.Flags.StringVar(&c.failureSummaryFile, "failure_summary", "", "filename for failure summary (relative to -log_dir)")

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

	c.Flags.StringVar(&c.subtool, "t", "", "run a subtool (use '-t list' to list subtools)")
	c.Flags.BoolVar(&c.cleandead, "cleandead", false, "clean built files that are no longer produced by the manifest")
	c.Flags.Var(&c.debugMode, "d", "enable debugging (use '-d list' to list modes)")
	c.Flags.StringVar(&c.adjustWarn, "w", "", "adjust warnings. not supported b/288807840")
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
	if c.subtool == "" && c.dir != "." {
		ui.Default.PrintLines(fmt.Sprintf("ninja: Entering directory `%s'", c.dir))
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
		ui.Default.PrintLines(fmt.Sprintf("exec_root=%s dir=%s", execRoot, c.dir))
	}
	_, err = os.Stat(c.fname)
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("%s not found in %s. need `-C <dir>`?", c.fname, cwd)
	}
	return execRoot, err
}

func (c *ninjaCmdRun) initLogDir() error {
	if !filepath.IsAbs(c.logDir) {
		logDir, err := filepath.Abs(c.logDir)
		if err != nil {
			return fmt.Errorf("abspath for log dir: %w", err)
		}
		c.logDir = logDir
	}
	err := os.MkdirAll(c.logDir, 0755)
	if err != nil {
		return err
	}
	// err = c.logSymlink(ctx)
	// if err != nil {
	// 	log.Warnf("failed to create symlink for log: %v", err)
	// }
	return nil
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

func (c *ninjaCmdRun) initBuildOpts(projectID string, buildPath *build.Path, config *buildconfig.Config, ds dataSource, hashFS *hashfs.HashFS, limits build.Limits) (bopts build.Options, done func(*error), err error) {
	var dones []func(*error)
	defer func() {
		if err != nil {
			for i := len(dones) - 1; i >= 0; i++ {
				dones[i](&err)
			}
			dones = nil
		}
	}()

	failureSummaryWriter, done, err := c.logWriter(c.failureSummaryFile)
	if err != nil {
		return bopts, nil, err
	}
	dones = append(dones, done)

	dones = append(dones, func(errp *error) {
		if failureSummaryWriter != nil && *errp != nil {
			fmt.Fprintf(failureSummaryWriter, "error: %v\n", *errp)
		}
	})

	dones = append(dones, done)

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
		StartTime:            c.started,
		ProjectID:            projectID,
		Metadata:             config.Metadata,
		Path:                 buildPath,
		HashFS:               hashFS,
		REAPIClient:          ds.client,
		RECacheEnableRead:    c.reCacheEnableRead,
		ActionSalt:           actionSaltBytes,
		OutputLocal:          build.OutputLocalFunc(c.fsopt.OutputLocal),
		Cache:                cache,
		FailureSummaryWriter: failureSummaryWriter,
		Clobber:              c.clobber,
		Prepare:              c.prepare,
		Verbose:              c.verbose,
		VerboseFailures:      c.verboseFailures,
		DryRun:               c.dryRun,
		FailuresAllowed:      c.failuresAllowed,
		KeepRSP:              c.debugMode.Keeprsp,
		Limits:               limits,
	}
	return bopts, func(err *error) {
		for i := len(dones) - 1; i >= 0; i-- {
			dones[i](err)
		}
	}, nil
}

// logFilename returns siso's log filename relative to startDir, or absolute path.
func (c *ninjaCmdRun) logFilename(fname, startDir string) string {
	if fname == "" {
		return ""
	}
	if !filepath.IsAbs(fname) {
		fname = filepath.Join(c.logDir, fname)
	}
	if startDir == "" {
		return fname
	}
	rel, err := filepath.Rel(startDir, fname)
	if err != nil || !filepath.IsLocal(rel) {
		return fname
	}
	return "." + string(os.PathSeparator) + rel
}

// glogFilename returns filename of glog logfile. i.e. siso.INFO.
// func (c *ninjaCmdRun) glogFilename() string {
// 	logFilename := "siso.INFO"
// 	if runtime.GOOS == "windows" {
// 		logFilename = "siso.exe.INFO"
// 	}
// 	return filepath.Join(c.logDir, logFilename)
// }

func (c *ninjaCmdRun) logWriter(fname string) (io.Writer, func(errp *error), error) {
	fname = c.logFilename(fname, "")
	if fname == "" {
		return nil, func(*error) {}, nil
	}
	rotateFiles(fname)
	f, err := os.Create(fname)
	if err != nil {
		return nil, func(*error) {}, err
	}
	return f, func(errp *error) {
		cerr := f.Close()
		if *errp == nil {
			*errp = cerr
		}
	}, nil
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
	mfbopts.Prepare = false
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

func doBuild(ctx context.Context, graph *ninjabuild.Graph, bopts build.Options, nopts runNinjaOpts, args ...string) (stats build.Stats, err error) {
	err = rebuildManifest(ctx, graph, bopts)
	if err != nil {
		return stats, err
	}

	if !bopts.DryRun && nopts.cleandead {
		spin := ui.Default.NewSpinner()
		spin.Start("cleaning deadfiles")
		n, total, err := graph.CleanDead(ctx)
		if err != nil {
			spin.Stop(err)
			return stats, err
		}
		if nopts.subtool == "cleandead" {
			spin.Done("%d/%d generated files", n, total)
			return stats, nil
		}
		spin.Stop(nil)
	}

	b, err := build.New(ctx, graph, bopts)
	if err != nil {
		return stats, err
	}
	hctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if nopts.enableStatusz {
		go func() {
			err := newStatuszServer(hctx, b)
			if err != nil {
				log.Warnf("statusz: %v", err)
			}
		}()
	}

	defer func(ctx context.Context) {
		cerr := b.Close()
		if cerr != nil {
			log.Warnf("failed to close builder: %v", cerr)
		}
	}(ctx)
	// prof := newCPUProfiler(ctx, "build")
	err = b.Build(ctx, "build", args...)
	// prof.stop(ctx)

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

// func (c *ninjaCmdRun) logSymlink(ctx context.Context) error {
// 	logFilename := c.glogFilename()
// 	rotateFiles(ctx, logFilename)
// 	logfiles, err := log.Names("INFO")
// 	if err != nil {
// 		return fmt.Errorf("failed to get glog INFO level log files: %w", err)
// 	}
// 	if len(logfiles) == 0 {
// 		return fmt.Errorf("no glog INFO level log files")
// 	}
// 	err = os.Symlink(logfiles[0], logFilename)
// 	if err != nil {
// 		log.Warnf("failed to create %s: %v", logFilename, err)
// 		// On Windows, it failed to create symlink.
// 		// just same filename in *.redirected file.
// 		err = os.WriteFile(logFilename+".redirected", []byte(logfiles[0]), 0644)
// 		if err != nil {
// 			log.Warnf("failed to write %s.redirected: %v", logFilename, err)
// 		}
// 		c.sisoInfoLog = logfiles[0]
// 		return nil
// 	}
// 	log.Infof("logfile: %q", logfiles)
// 	c.sisoInfoLog = filepath.Base(logFilename)
// 	return nil
// }

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

func rotateFiles(fname string) {
	ext := filepath.Ext(fname)
	fnameBase := strings.TrimSuffix(fname, ext)
	for i := 8; i >= 0; i-- {
		err := os.Rename(
			fmt.Sprintf("%s.%d%s", fnameBase, i, ext),
			fmt.Sprintf("%s.%d%s", fnameBase, i+1, ext))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			log.Warnf("rotate %s %d->%d failed: %v", fname, i, i+1, err)
		}
	}
	err := os.Rename(fname, fmt.Sprintf("%s.0%s", fnameBase, ext))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Warnf("rotate %s ->0 failed: %v", fname, err)
	}
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
			// .h,/.hxx/.hpp/.inc/.c/.cc/.cxx/.cpp/.m/.mm for gcc deps or msvc showIncludes
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

type lastTargets struct {
	Targets []string `json:"targets,omitempty"`
	Failed  []string `json:"failed,omitempty"`
}

func loadTargets(targetsFile string) ([]string, []string, error) {
	buf, err := os.ReadFile(targetsFile)
	if err != nil {
		return nil, nil, err
	}
	var last lastTargets
	err = json.Unmarshal(buf, &last)
	if err != nil {
		return nil, nil, fmt.Errorf("parse error %s: %w", targetsFile, err)
	}
	return last.Targets, last.Failed, nil
}

func saveTargets(targetsFile string, targets, failed []string) error {
	v := lastTargets{
		Targets: targets,
		Failed:  failed,
	}
	buf, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal last targets: %w", err)
	}
	err = os.WriteFile(targetsFile, buf, 0644)
	if err != nil {
		return fmt.Errorf("save last targets: %w", err)
	}
	return nil
}

func checkTargets(lastTargetsFilename string, targets []string) ([]string, bool) {
	lastTargets, failed, err := loadTargets(lastTargetsFilename)
	if err != nil {
		log.Warnf("checkTargets: %v", err)
		return nil, false
	}
	if len(targets) != len(lastTargets) {
		return nil, false
	}
	sort.Strings(targets)
	sort.Strings(lastTargets)
	for i := range targets {
		if targets[i] != lastTargets[i] {
			return nil, false
		}
	}
	return failed, true
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
		ui.Default.PrintLines(ui.SGR(ui.BackgroundRed, fmt.Sprintf("Garbage collection is disabled. GOGC=%s\n", os.Getenv("GOGC"))))
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

func (c *ninjaCmdRun) setupCrashOutput() (func(), error) {
	fname := c.logFilename("siso_crash", "")
	rotateFiles(fname)
	crashFile, err := os.Create(fname)
	if err != nil {
		return nil, err
	}
	err = debug.SetCrashOutput(crashFile, debug.CrashOptions{})
	if err != nil {
		return nil, err
	}
	return func() { debug.SetCrashOutput(nil, debug.CrashOptions{}) }, crashFile.Close()
}
