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
	"runtime"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"cloud.google.com/go/logging"
	"cloud.google.com/go/profiler"
	log "github.com/golang/glog"
	"github.com/google/uuid"
	"github.com/maruel/subcommands"
	"golang.org/x/sync/errgroup"
	"google.golang.org/api/option"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/grpc/grpclog"

	"go.chromium.org/luci/auth"
	"go.chromium.org/luci/cipd/version"
	"go.chromium.org/luci/common/cli"
	"go.chromium.org/luci/common/system/signals"

	"infra/build/siso/auth/cred"
	"infra/build/siso/build"
	"infra/build/siso/build/buildconfig"
	"infra/build/siso/build/ninjabuild"
	"infra/build/siso/hashfs"
	"infra/build/siso/o11y/clog"
	"infra/build/siso/o11y/trace"
	"infra/build/siso/reapi"
	"infra/build/siso/reapi/digest"
	"infra/build/siso/sync/semaphore"
	"infra/build/siso/toolsupport/ninjautil"
	"infra/build/siso/ui"
)

// Cmd returns the Command for the `ninja` subcommand provided by this package.
func Cmd(authOpts cred.Options) *subcommands.Command {
	return &subcommands.Command{
		UsageLine: "ninja <args>...",
		CommandRun: func() subcommands.CommandRun {
			r := ninjaCmdRun{
				authOpts: authOpts,
			}
			r.init()
			return &r
		},
	}
}

type ninjaCmdRun struct {
	subcommands.CommandRunBase
	authOpts cred.Options
	started  time.Time

	// flag values
	dir        string
	configName string
	projectID  string

	jobID string

	offline         bool
	batch           bool
	verbose         bool
	dryRun          bool
	clobber         bool
	failuresAllowed int
	actionSalt      string

	ninjaJobs  int
	remoteJobs int
	fname      string

	cacheDir         string
	localCacheEnable bool
	cacheEnableRead  bool
	// cacheEnableWrite bool

	configRepoDir  string
	configFilename string

	outputLocalStrategy string

	depsLogFile string
	// depsLogBucket

	logDir             string
	failureSummaryFile string
	outputLogFile      string
	explainFile        string
	localexecLogFile   string
	metricsJSON        string
	traceJSON          string
	buildPprof         string
	// uploadBuildPprof bool

	fsopt             *hashfs.Option
	reopt             *reapi.Option
	reCacheEnableRead bool
	// reCacheEnableWrite bool
	reproxyAddr string

	enableCloudLogging bool
	// enableCPUProfiler bool
	enableCloudProfiler      bool
	cloudProfilerServiceName string
	enableCloudTrace         bool
	traceThreshold           time.Duration
	traceSpanThreshold       time.Duration

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
			msgPrefix := "Build Failure"
			if ui.IsTerminal() {
				dur = ui.SGR(ui.Bold, dur)
				msgPrefix = ui.SGR(ui.BackgroundRed, msgPrefix)
			}
			fmt.Fprintf(os.Stderr, "%6s %s: %d done %d failed %d remaining - %.02f/s\n %v\n", dur, msgPrefix, stats.Done-stats.Skipped, stats.Fail, stats.Total-stats.Done, sps, errBuild.err)
			suggest := fmt.Sprintf("see %s for command output", c.logFilename(c.outputLogFile))
			if c.sisoInfoLog != "" {
				suggest += fmt.Sprintf("\n or %s", c.logFilename(c.sisoInfoLog))
			}
			if ui.IsTerminal() {
				suggest = ui.SGR(ui.Bold, suggest)
			}
			fmt.Fprintf(os.Stderr, "%s\n", suggest)
		default:
			msgPrefix := "Error"
			if ui.IsTerminal() {
				msgPrefix = ui.SGR(ui.BackgroundRed, msgPrefix)
			}
			fmt.Fprintf(os.Stderr, "%6s %s: %v\n", ui.FormatDuration(time.Since(c.started)), msgPrefix, err)
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
		var i int
		for i = 0; i < len(args); i++ {
			arg := args[i]
			if !strings.HasPrefix(arg, "-") {
				targets = append(targets, arg)
				continue
			}
			err := flagSet.Parse(args[i:])
			if err != nil {
				return err
			}
			break
		}
		if i == len(args) {
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

const (
	lastTargetsFile   = ".siso_last_targets"
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

	limits := build.DefaultLimits(ctx)
	if c.remoteJobs > 0 {
		limits.Remote = c.remoteJobs
		limits.REWrap = c.remoteJobs
	}
	if c.ninjaJobs >= 0 {
		fmt.Fprintf(os.Stderr, "-j is specified. but not supported. b/288829511\n")
	}
	if c.failuresAllowed == 0 {
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
		clog.Warningf(ctx, "offline mode")
		c.reopt = new(reapi.Option)
		c.projectID = ""
		c.enableCloudLogging = false
		c.enableCloudProfiler = false
		c.enableCloudTrace = false
		c.reproxyAddr = ""
	}

	execRoot, err := c.initWorkdirs(ctx)
	if err != nil {
		return stats, err
	}
	buildPath := build.NewPath(execRoot, c.dir)

	buildID := uuid.New().String()
	projectID := c.reopt.UpdateProjectID(c.projectID)

	var credential cred.Cred
	if projectID != "" {
		// TODO: can be async until cred is needed?
		spin := ui.Default.NewSpinner()
		spin.Start("init credentials")
		credential, err = cred.New(ctx, c.authOpts)
		if err != nil {
			spin.Stop(errors.New(""))
			return stats, err
		}
		spin.Stop(nil)
	}
	if c.enableCloudLogging {
		logCtx, loggerURL, done, err := c.initCloudLogging(ctx, projectID, buildID, execRoot, credential)
		if err != nil {
			return stats, err
		}
		// use stderr for confirm no-op step. b/288534744
		fmt.Fprintln(os.Stderr, loggerURL)
		defer done()
		ctx = logCtx
	}
	// logging is ready.

	if cmdver, err := version.GetStartupVersion(); err != nil {
		clog.Warningf(ctx, "cannot determine CIPD package version: %s", err)
	} else {
		clog.Infof(ctx, "CIPD package name: %s", cmdver.PackageName)
		clog.Infof(ctx, "CIPD instance ID: %s", cmdver.InstanceID)
	}

	clog.Infof(ctx, "build id: %q", buildID)
	clog.Infof(ctx, "project id: %q", projectID)
	clog.Infof(ctx, "commandline %q", os.Args)

	if c.enableCloudProfiler {
		c.initCloudProfiler(ctx, projectID, credential)
	}
	var traceExporter *trace.Exporter
	if c.enableCloudTrace {
		traceExporter = c.initCloudTrace(ctx, projectID, credential)
		defer traceExporter.Close(ctx)
	}
	// upload build pprof

	err = c.initLogDir(ctx)
	if err != nil {
		return stats, err
	}

	targets := c.Flags.Args()
	config, err := c.initConfig(ctx, execRoot, targets)
	if err != nil {
		return stats, err
	}
	sameTargets := checkTargets(ctx, lastTargetsFile, targets)

	spin := ui.Default.NewSpinner()

	var eg errgroup.Group
	var localDepsLog *ninjautil.DepsLog
	eg.Go(func() error {
		depsLog, err := c.initDepsLog(ctx)
		if err != nil {
			return err
		}
		localDepsLog = depsLog
		return nil
	})

	if c.reopt.IsValid() {
		ui.Default.PrintLines(fmt.Sprintf("reapi instance: %s\n", c.reopt.Instance))
	}
	ds, err := c.initDataSource(ctx, credential)
	if err != nil {
		return stats, err
	}
	defer func() {
		err := ds.Close(ctx)
		if err != nil {
			clog.Errorf(ctx, "close datasource: %v", err)
		}
	}()
	c.fsopt.DataSource = ds
	c.fsopt.OutputLocal, err = c.initOutputLocal()
	if err != nil {
		return stats, err
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
		if err != nil {
			if !c.batch {
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
			clog.Infof(ctx, "record failed targets: %q", stepError.Target)
			serr := saveTargets(ctx, failedTargetsFile, []string{stepError.Target})
			if serr != nil {
				clog.Warningf(ctx, "failed to save failed targets: %v", serr)
				return
			}
			// when write failedTargetsFile, need to write lastTargetsFile too.
		}
		clog.Infof(ctx, "save targets to %s...", lastTargetsFile)
		serr := saveTargets(ctx, lastTargetsFile, targets)
		if serr != nil {
			clog.Warningf(ctx, "failed to save last targets: %v", serr)
		}
	}()
	defer func() {
		err := hashFS.Close(ctx)
		if err != nil {
			clog.Errorf(ctx, "close hashfs: %v", err)
		}
	}()

	_, err = os.Stat(failedTargetsFile)
	lastFailed := err == nil
	clog.Infof(ctx, "sameTargets: %t hashfs clean: %t last failed: %t", sameTargets, hashFS.IsClean(), lastFailed)
	if !c.clobber && !c.dryRun && !c.debugMode.Explain && sameTargets && hashFS.IsClean() && !lastFailed {
		// TODO: better to check digest of .siso_fs_state?
		return stats, errNothingToDo
	}
	os.Remove(lastTargetsFile)

	bopts, done, err := c.initBuildOpts(ctx, projectID, buildID, buildPath, config, ds, hashFS, limits, traceExporter)
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
	stepConfig, err := ninjabuild.NewStepConfig(ctx, config, buildPath, hashFS, "build.ninja")
	if err != nil {
		spin.Stop(err)
		return stats, err
	}
	spin.Stop(nil)

	spin.Start(fmt.Sprintf("load %s", c.fname))
	nstate, err := ninjabuild.Load(ctx, c.fname, buildPath)
	if err != nil {
		spin.Stop(errors.New(""))
		return stats, err
	}
	spin.Stop(nil)

	graph := ninjabuild.NewGraph(ctx, c.fname, nstate, config, buildPath, hashFS, stepConfig, localDepsLog)

	return runNinja(ctx, c.fname, graph, bopts, targets, c.dryRun, !c.batch && sameTargets && !c.clobber)
}

func runNinja(ctx context.Context, fname string, graph *ninjabuild.Graph, bopts build.Options, targets []string, dryRun, checkFailedTargets bool) (build.Stats, error) {
	var stats build.Stats
	spin := ui.Default.NewSpinner()

	for {
		clog.Infof(ctx, "build starts")
		if checkFailedTargets {
			failedTargets, err := loadTargets(ctx, failedTargetsFile)
			if err != nil {
				clog.Infof(ctx, "no failed targets: %v", err)
			} else {
				ui.Default.PrintLines(fmt.Sprintf("Building last failed targets: %s...\n", failedTargets))
				stats, err = doBuild(ctx, graph, bopts, failedTargets...)
				if errors.Is(err, build.ErrManifestModified) {
					if dryRun {
						return stats, nil
					}
					clog.Infof(ctx, "%s modified.", fname)
					spin.Start("reloading")
					err := graph.Reload(ctx)
					if err != nil {
						spin.Stop(err)
						return stats, err
					}
					spin.Stop(nil)
					ui.Default.PrintLines("\n", "\n")
					clog.Infof(ctx, "reload done. build retry")
					continue
				}
				if err != nil {
					return stats, err
				}
				os.Remove(failedTargetsFile)
				ui.Default.PrintLines(fmt.Sprintf(" %s: %s\n", ui.SGR(ui.Green, "last failed targets fixed"), failedTargets))
				continue
			}
			graph.Reset(ctx)
		}
		err := os.Remove(failedTargetsFile)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			clog.Warningf(ctx, "failed to remove %s: %v", failedTargetsFile, err)
		}
		stats, err := doBuild(ctx, graph, bopts, targets...)
		if errors.Is(err, build.ErrManifestModified) {
			if dryRun {
				return stats, nil
			}
			clog.Infof(ctx, "%s modified", fname)
			spin.Start("reloading")
			err := graph.Reload(ctx)
			if err != nil {
				spin.Stop(err)
				return stats, err
			}
			spin.Stop(nil)
			clog.Infof(ctx, "reload done. build retry")
			continue
		}
		clog.Infof(ctx, "build finished: %v", err)
		return stats, err
	}
}

func (c *ninjaCmdRun) init() {
	c.Flags.StringVar(&c.dir, "C", ".", "ninja running directory")
	c.Flags.StringVar(&c.configName, "config", "", "config name passed to starlark")
	c.Flags.StringVar(&c.projectID, "project", os.Getenv("SISO_PROJECT"), "cloud project ID. can set by $SISO_PROJECT")

	c.Flags.StringVar(&c.jobID, "job_id", uuid.New().String(), "job id for a grouping of related builds. used for cloud logging resource labels job (truncated to 1024), or correlated_invocations_id for remote-apis request metadata")

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
	c.Flags.BoolVar(&c.dryRun, "n", false, "dry run")
	c.Flags.BoolVar(&c.clobber, "clobber", false, "clobber build")
	c.Flags.IntVar(&c.failuresAllowed, "k", 1, "keep going until N jobs fail (0 means inifinity)")
	c.Flags.StringVar(&c.actionSalt, "action_salt", "", "action salt")

	c.Flags.IntVar(&c.ninjaJobs, "j", -1, "run N jobs in parallel (0 means infinity). not supported b/288829511")
	c.Flags.IntVar(&c.remoteJobs, "remote_jobs", 0, "run N remote jobs in parallel. when the value is no positive, the default will be computed based on # of CPUs.")
	c.Flags.StringVar(&c.fname, "f", "build.ninja", "input build manifet filename (relative to -C)")

	c.Flags.StringVar(&c.cacheDir, "cache_dir", defaultCacheDir(), "cache directory")
	c.Flags.BoolVar(&c.localCacheEnable, "local_cache_enable", false, "local cache enable")
	c.Flags.BoolVar(&c.cacheEnableRead, "cache_enable_read", true, "cache enable read")

	c.Flags.StringVar(&c.configRepoDir, "config_repo_dir", "build/config/siso", "config repo directory (relative to exec root)")
	c.Flags.StringVar(&c.configFilename, "load", "@config//main.star", "config filename (@config// is --config_repo_dir)")
	c.Flags.StringVar(&c.outputLocalStrategy, "output_local_strategy", "full", `strategy for output_local. "full": download all outputs. "greedy": downloads most outputs except intermediate objs. "minimum": downloads as few as possible`)
	c.Flags.StringVar(&c.depsLogFile, "deps_log", ".siso_deps", "deps log filename (relative to -C)")

	c.Flags.StringVar(&c.logDir, "log_dir", ".", "log directory (relative to -C")
	c.Flags.StringVar(&c.failureSummaryFile, "failure_summary", "", "filename for failure summary (relative to -log_dir)")
	c.Flags.StringVar(&c.outputLogFile, "output_log", "siso_output", "output log filename (relative to -log_dir")
	c.Flags.StringVar(&c.explainFile, "explain_log", "siso_explain", "explain log filename (relative to -log_dir")
	c.Flags.StringVar(&c.localexecLogFile, "localexec_log", "siso_localexec", "localexec log filename (relative to -log_dir")
	c.Flags.StringVar(&c.metricsJSON, "metrics_json", "siso_metrics.json", "metrics JSON filename (relative to -log_dir)")
	c.Flags.StringVar(&c.traceJSON, "trace_json", "siso_trace.json", "trace JSON filename (relative to -log_dir)")
	c.Flags.StringVar(&c.buildPprof, "build_pprof", "siso_build.pprof", "build pprof filename (relative to -log_dir)")

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
	// reclient_helper.py sets the RBE_server_address
	// https://chromium.googlesource.com/chromium/tools/depot_tools.git/+/e13840bd9a04f464e3bef22afac1976fc15a96a0/reclient_helper.py#138
	c.reproxyAddr = os.Getenv("RBE_server_address")

	c.Flags.DurationVar(&c.traceThreshold, "trace_threshold", 1*time.Minute, "threshold for trace record")
	c.Flags.DurationVar(&c.traceSpanThreshold, "trace_span_threshold", 100*time.Millisecond, "theshold for trace span record")

	c.Flags.BoolVar(&c.enableCloudLogging, "enable_cloud_logging", false, "enable cloud logging")
	c.Flags.BoolVar(&c.enableCloudProfiler, "enable_cloud_profiler", false, "enable cloud profiler")
	c.Flags.StringVar(&c.cloudProfilerServiceName, "cloud_profiler_service_name", "siso", "cloud profiler service name")
	c.Flags.BoolVar(&c.enableCloudTrace, "enable_cloud_trace", false, "enable cloud trace")

	c.Flags.Var(&c.debugMode, "d", "enable debugging (use '-d list' to list modes)")
	c.Flags.StringVar(&c.adjustWarn, "w", "", "adjust warnings. not supported b/288807840")
}

func (c *ninjaCmdRun) initWorkdirs(ctx context.Context) (string, error) {
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
	clog.Infof(ctx, "wd: %s", execRoot)
	if !filepath.IsAbs(c.configRepoDir) {
		execRoot, err = detectExecRoot(ctx, execRoot, c.configRepoDir)
		if err != nil {
			return "", err
		}
		c.configRepoDir = filepath.Join(execRoot, c.configRepoDir)
	}
	clog.Infof(ctx, "exec_root: %s", execRoot)

	err = os.Chdir(c.dir)
	if err != nil {
		return "", err
	}
	clog.Infof(ctx, "change dir to %s", c.dir)
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
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
	clog.Infof(ctx, "working_directory in exec_root: %s", c.dir)
	return execRoot, nil
}

func (c *ninjaCmdRun) initCloudLogging(ctx context.Context, projectID, buildID, execRoot string, credential cred.Cred) (context.Context, string, func(), error) {
	log.Infof("enable cloud logging project=%s id=%s", projectID, buildID)

	// log_id: "siso.log" and "siso.step"
	// use generic_task resource
	// https://cloud.google.com/logging/docs/api/v2/resource-list
	// https://cloud.google.com/monitoring/api/resources#tag_generic_task
	client, err := logging.NewClient(ctx, projectID, credential.ClientOptions()...)
	if err != nil {
		return ctx, "", func() {}, err
	}
	hostname, err := os.Hostname()
	if err != nil {
		return ctx, "", func() {}, err
	}
	// Monitored resource labels have a maximum length of 1024. b/295251052
	job := c.jobID
	if len(job) > 1024 {
		job = job[:1024]
	}
	logger, err := clog.New(ctx, client, "siso.log", "siso.step", &mrpb.MonitoredResource{
		Type: "generic_task",
		Labels: map[string]string{
			"project_id": projectID,
			"location":   hostname,
			"namespace":  execRoot,
			"job":        job,
			"task_id":    buildID,
		},
	})
	if err != nil {
		return ctx, "", func() {}, err
	}
	ctx = clog.NewContext(ctx, logger)
	grpclog.SetLoggerV2(logger)
	return ctx, logger.URL(), func() {
		err := logger.Close()
		if err != nil {
			// Don't use clog as it's closing Cloud logging client.
			log.Warningf("falied to close Cloud logger: %v", err)
		}
	}, nil
}

func (c *ninjaCmdRun) initCloudProfiler(ctx context.Context, projectID string, credential cred.Cred) {
	clog.Infof(ctx, "enable cloud profiler %q in %s", c.cloudProfilerServiceName, projectID)
	err := profiler.Start(profiler.Config{
		Service:        c.cloudProfilerServiceName,
		MutexProfiling: true,
		ProjectID:      projectID,
	}, credential.ClientOptions()...)
	if err != nil {
		clog.Errorf(ctx, "failed to start cloud profiler: %v", err)
	}
}

func (c *ninjaCmdRun) initCloudTrace(ctx context.Context, projectID string, credential cred.Cred) *trace.Exporter {
	clog.Infof(ctx, "enable trace in %s [trace > %s]", projectID, c.traceThreshold)
	traceExporter, err := trace.NewExporter(ctx, trace.Options{
		ProjectID:     projectID,
		StepThreshold: c.traceThreshold,
		SpanThreshold: c.traceSpanThreshold,
		ClientOptions: append([]option.ClientOption{}, credential.ClientOptions()...),
	})
	if err != nil {
		clog.Errorf(ctx, "failed to start trace exporter: %v", err)
	}
	return traceExporter
}

func (c *ninjaCmdRun) initLogDir(ctx context.Context) error {
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
	return c.logSymlink(ctx)
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
		clog.Warningf(ctx, "no args.gn: %v", err)
	} else {
		return nil, err
	}
	return config, nil
}

func (c *ninjaCmdRun) initDepsLog(ctx context.Context) (*ninjautil.DepsLog, error) {
	err := os.MkdirAll(filepath.Dir(c.depsLogFile), 0755)
	if err != nil {
		clog.Warningf(ctx, "failed to mkdir for deps log: %v", err)
		return nil, err
	}
	depsLog, err := ninjautil.NewDepsLog(ctx, c.depsLogFile)
	if err != nil {
		clog.Warningf(ctx, "failed to load deps log: %v", err)
		return nil, err
	}
	if !depsLog.NeedsRecompact() {
		return depsLog, nil
	}
	err = depsLog.Recompact(ctx)
	if err != nil {
		clog.Warningf(ctx, "failed to recompact deps log: %v", err)
		return nil, err
	}
	return depsLog, nil
}

func (c *ninjaCmdRun) initBuildOpts(ctx context.Context, projectID, buildID string, buildPath *build.Path, config *buildconfig.Config, ds dataSource, hashFS *hashfs.HashFS, limits build.Limits, traceExporter *trace.Exporter) (bopts build.Options, done func(*error), err error) {
	var dones []func(*error)
	defer func() {
		if err != nil {
			for i := len(dones) - 1; i >= 0; i++ {
				dones[i](&err)
			}
			dones = nil
		}
	}()

	failureSummaryWriter, done, err := c.logWriter(ctx, c.failureSummaryFile)
	if err != nil {
		return bopts, nil, err
	}
	dones = append(dones, done)
	dones = append(dones, func(errp *error) {
		if failureSummaryWriter != nil && *errp != nil {
			fmt.Fprintf(failureSummaryWriter, "error: %v\n", *errp)
		}
	})
	outputLogWriter, done, err := c.logWriter(ctx, c.outputLogFile)
	if err != nil {
		return bopts, nil, err
	}
	dones = append(dones, done)
	explainWriter, done, err := c.logWriter(ctx, c.explainFile)
	if err != nil {
		return bopts, nil, err
	}
	dones = append(dones, done)
	if c.debugMode.Explain {
		if explainWriter == nil {
			explainWriter = newExplainWriter(os.Stderr, "")
		} else {
			explainWriter = io.MultiWriter(newExplainWriter(os.Stderr, filepath.Join(c.dir, c.explainFile)), explainWriter)
		}
	}

	localexecLogWriter, done, err := c.logWriter(ctx, c.localexecLogFile)
	if err != nil {
		return bopts, nil, err
	}
	dones = append(dones, done)

	metricsJSONWriter, done, err := c.logWriter(ctx, c.metricsJSON)
	if err != nil {
		return bopts, nil, err
	}
	dones = append(dones, done)

	if !filepath.IsAbs(c.traceJSON) {
		c.traceJSON = filepath.Join(c.logDir, c.traceJSON)
	}
	if !filepath.IsAbs(c.buildPprof) {
		c.buildPprof = filepath.Join(c.logDir, c.buildPprof)
	}

	ninjaLogWriter, err := ninjautil.OpenNinjaLog(ctx)
	if err != nil {
		return bopts, nil, err
	}
	dones = append(dones, func(errp *error) {
		clog.Infof(ctx, "close .ninja_log")
		cerr := ninjaLogWriter.Close()
		if *errp == nil {
			*errp = cerr
		}
	})
	var actionSaltBytes []byte
	if c.actionSalt != "" {
		actionSaltBytes = []byte(c.actionSalt)
	}
	if c.traceJSON != "" {
		rotateFiles(ctx, c.traceJSON)
	}

	cache, err := build.NewCache(ctx, build.CacheOptions{
		Store:      ds.cache,
		EnableRead: c.cacheEnableRead,
	})
	if err != nil {
		clog.Warningf(ctx, "no cache enabled: %v", err)
	}
	bopts = build.Options{
		JobID:                c.jobID,
		ID:                   buildID,
		StartTime:            c.started,
		ProjectID:            projectID,
		Metadata:             config.Metadata,
		Path:                 buildPath,
		HashFS:               hashFS,
		REAPIClient:          ds.client,
		RECacheEnableRead:    c.reCacheEnableRead,
		ReproxyAddr:          c.reproxyAddr,
		ActionSalt:           actionSaltBytes,
		OutputLocal:          build.OutputLocalFunc(c.fsopt.OutputLocal),
		Cache:                cache,
		FailureSummaryWriter: failureSummaryWriter,
		OutputLogWriter:      outputLogWriter,
		ExplainWriter:        explainWriter,
		LocalexecLogWriter:   localexecLogWriter,
		MetricsJSONWriter:    metricsJSONWriter,
		NinjaLogWriter:       ninjaLogWriter,
		TraceExporter:        traceExporter,
		TraceJSON:            c.traceJSON,
		Pprof:                c.buildPprof,
		Clobber:              c.clobber,
		Verbose:              c.verbose,
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

// logFilename returns siso's log filename relative to start dir, or absolute path.
func (c *ninjaCmdRun) logFilename(fname string) string {
	if !filepath.IsAbs(fname) {
		fname = filepath.Join(c.logDir, fname)
	}
	rel, err := filepath.Rel(c.startDir, fname)
	if err != nil || !filepath.IsLocal(rel) {
		return fname
	}
	return rel
}

// glogFilename returns filename of glog logfile. i.e. siso.INFO.
func (c *ninjaCmdRun) glogFilename() string {
	logFilename := "siso.INFO"
	if runtime.GOOS == "windows" {
		logFilename = "siso.exe.INFO"
	}
	return filepath.Join(c.logDir, logFilename)
}

func (c *ninjaCmdRun) logWriter(ctx context.Context, fname string) (io.Writer, func(errp *error), error) {
	if fname == "" {
		return nil, func(*error) {}, nil
	}
	if !filepath.IsAbs(fname) {
		fname = filepath.Join(c.logDir, fname)
	}
	rotateFiles(ctx, fname)
	f, err := os.Create(fname)
	if err != nil {
		return nil, func(*error) {}, err
	}
	return f, func(errp *error) {
		clog.Infof(ctx, "close %s", fname)
		cerr := f.Close()
		if *errp == nil {
			*errp = cerr
		}
	}, nil
}

func defaultCacheDir() string {
	d, err := os.UserCacheDir()
	if err != nil {
		log.Warningf("Failed to get user cache dir: %v", err)
		return ""
	}
	return filepath.Join(d, "siso")
}

func doBuild(ctx context.Context, graph *ninjabuild.Graph, bopts build.Options, args ...string) (stats build.Stats, err error) {
	clog.Infof(ctx, "rebuild manifest")
	mfbopts := bopts
	mfbopts.Clobber = false
	mfbopts.RebuildManifest = graph.Filename()
	mfb, err := build.New(ctx, graph, mfbopts)
	if err != nil {
		return stats, err
	}
	err = mfb.Build(ctx, "rebuild manifest", graph.Filename())
	cerr := mfb.Close()
	if cerr != nil {
		return stats, fmt.Errorf("failed to close builder: %w", cerr)
	}
	if err != nil {
		return stats, err
	}
	// TODO(b/266518906): upload manifest

	b, err := build.New(ctx, graph, bopts)
	if err != nil {
		return stats, err
	}
	defer func(ctx context.Context) {
		cerr := b.Close()
		if cerr != nil {
			clog.Warningf(ctx, "failed to close builder: %v", cerr)
		}
	}(ctx)
	// prof := newCPUProfiler(ctx, "build")
	err = b.Build(ctx, "build", args...)
	// prof.stop(ctx)

	semaTraces := make(map[string]semaTrace)
	tstats := b.TraceStats()
	var rbeWorker, rbeExec *build.TraceStat
	for _, ts := range tstats {
		clog.Infof(ctx, "%s: n=%d avg=%s max=%s", ts.Name, ts.N, ts.Avg(), ts.Max)
		switch {
		case strings.HasPrefix(ts.Name, "wait:"):
			name := strings.TrimPrefix(ts.Name, "wait:")
			t := semaTraces[name]
			t.name = name
			t.n = ts.N
			t.waitAvg = ts.Avg()
			t.waitBuckets = ts.Buckets
			semaTraces[name] = t
		case strings.HasPrefix(ts.Name, "serv:"):
			name := strings.TrimPrefix(ts.Name, "serv:")
			t := semaTraces[name]
			t.name = name
			t.n = ts.N
			t.nerr = ts.NErr
			t.servAvg = ts.Avg()
			t.servBuckets = ts.Buckets
			semaTraces[name] = t
		case ts.Name == "rbe:queue":
			name := "rbe:sched"
			t := semaTraces[name]
			t.name = name
			t.n = ts.N
			t.nerr = ts.NErr
			t.waitAvg = ts.Avg()
			t.waitBuckets = ts.Buckets
			semaTraces[name] = t
		case ts.Name == "rbe:worker":
			rbeWorker = ts
		case ts.Name == "rbe:exec":
			rbeExec = ts
		}
	}
	if rbeWorker != nil {
		name := "rbe:sched"
		t := semaTraces[name]
		t.name = name
		t.servAvg = rbeWorker.Avg()
		t.servBuckets = rbeWorker.Buckets
		semaTraces[name] = t
	}
	if rbeWorker != nil && rbeExec != nil {
		name := "rbe:worker"
		t := semaTraces[name]
		t.name = name
		t.n = rbeExec.N
		t.waitAvg = rbeWorker.Avg() - rbeExec.Avg()
		// number of waits would not be correct with this calculation
		// because it just uses counts in buckets.
		// not sure how we can measure actual waiting time in buckets,
		// but this would provide enough estimated values.
		for i := range rbeWorker.Buckets {
			t.waitBuckets[i] = rbeWorker.Buckets[i] - rbeExec.Buckets[i]
		}
		t.servAvg = rbeExec.Avg()
		t.servBuckets = rbeExec.Buckets
		semaTraces[name] = t
	}
	if len(semaTraces) > 0 {
		dumpResourceUsageTable(ctx, semaTraces)
	}
	stats = b.Stats()
	clog.Infof(ctx, "stats=%#v", stats)
	if err != nil {
		return stats, buildError{err: err}
	}
	if bopts.REAPIClient == nil {
		return stats, err
	}
	// TODO(b/266518906): wait for completion of uploading manifest
	return stats, err
}

func detectExecRoot(ctx context.Context, execRoot, crdir string) (string, error) {
	for {
		_, err := os.Stat(filepath.Join(execRoot, crdir))
		if err == nil {
			return execRoot, nil
		}
		dir := filepath.Dir(execRoot)
		if dir == execRoot {
			// reached to root dir
			return "", fmt.Errorf("can not detect exec_root: %s not found", crdir)
		}
		execRoot = dir
	}
}

func dumpResourceUsageTable(ctx context.Context, semaTraces map[string]semaTrace) {
	var semaNames []string
	for key := range semaTraces {
		semaNames = append(semaNames, key)
	}
	sort.Strings(semaNames)
	var lsb, usb strings.Builder
	var needToShow bool
	ltw := tabwriter.NewWriter(&lsb, 10, 8, 1, ' ', tabwriter.AlignRight)
	utw := tabwriter.NewWriter(&usb, 10, 8, 1, ' ', tabwriter.AlignRight)
	fmt.Fprintf(ltw, "resource/capa\tused(err)\twait-avg\t|   s m |\tserv-avg\t|   s m |\t\n")
	fmt.Fprintf(utw, "resource/capa\tused(err)\twait-avg\t|   s m |\tserv-avg\t|   s m |\t\n")
	for _, key := range semaNames {
		t := semaTraces[key]
		s, _ := semaphore.Lookup(t.name)
		c := "nil"
		if s != nil {
			c = strconv.Itoa(s.Capacity())
		}
		fmt.Fprintf(ltw, "%s/%s\t%d(%d)\t%s\t%s\t%s\t%s\t\n", t.name, c, t.n, t.nerr, t.waitAvg.Round(time.Millisecond), histogram(t.waitBuckets), t.servAvg.Round(time.Millisecond), histogram(t.servBuckets))
		// bucket 5 = [1m,10m)
		// bucket 6 = [10m,*)
		if t.waitBuckets[5] > 0 || t.waitBuckets[6] > 0 || t.servBuckets[5] > 0 || t.servBuckets[6] > 0 {
			needToShow = true
			fmt.Fprintf(utw, "%s/%s\t%d(%d)\t%s\t%s\t%s\t%s\t\n", t.name, c, t.n, t.nerr, ui.FormatDuration(t.waitAvg), histogram(t.waitBuckets), ui.FormatDuration(t.servAvg), histogram(t.servBuckets))
		}
	}
	ltw.Flush()
	utw.Flush()
	if needToShow {
		fmt.Print(usb.String())
	}
	clog.Infof(ctx, "resource usage table:\n%s", lsb.String())
}

var histchar = [...]string{"▂", "▃", "▄", "▅", "▆", "▇", "█"}

func histogram(b [7]int) string {
	max := 0
	for _, n := range b {
		if max < n {
			max = n
		}
	}
	var sb strings.Builder
	sb.WriteRune('|')
	for _, n := range b {
		if n <= 0 {
			sb.WriteRune(' ')
			continue
		}
		i := len(histchar) * n / (max + 1)
		sb.WriteString(histchar[i])
	}
	sb.WriteRune('|')
	return sb.String()
}

type semaTrace struct {
	name                     string
	n, nerr                  int
	waitAvg, servAvg         time.Duration
	waitBuckets, servBuckets [7]int
}

func (c *ninjaCmdRun) logSymlink(ctx context.Context) error {
	logFilename := c.glogFilename()
	rotateFiles(ctx, logFilename)
	logfiles, err := log.Names("INFO")
	if err != nil {
		return fmt.Errorf("failed to get glog INFO level log files: %w", err)
	}
	if len(logfiles) == 0 {
		return fmt.Errorf("no glog INFO level log files")
	}
	err = os.Symlink(logfiles[0], logFilename)
	if err != nil {
		clog.Warningf(ctx, "failed to create %s: %v", logFilename, err)
		c.sisoInfoLog = logfiles[0]
		return nil
	}
	clog.Infof(ctx, "logfile: %q", logfiles)
	c.sisoInfoLog = filepath.Base(logFilename)
	return nil
}

type dataSource struct {
	cache  build.CacheStore
	client *reapi.Client
}

func (c *ninjaCmdRun) initDataSource(ctx context.Context, credential cred.Cred) (dataSource, error) {
	if !c.localCacheEnable {
		c.cacheDir = ""
	}
	var ds dataSource
	var err error
	ds.cache, err = build.NewLocalCache(c.cacheDir)
	if err != nil {
		clog.Warningf(ctx, "no local cache enabled: %v", err)
	}
	if c.reopt.IsValid() {
		ds.client, err = reapi.New(ctx, credential, *c.reopt)
		if err != nil {
			return ds, err
		}
		ds.cache = ds.client.CacheStore()
	}
	return ds, nil
}

func (ds dataSource) Close(ctx context.Context) error {
	if ds.client == nil {
		return nil
	}
	return ds.client.Close()
}

func (ds dataSource) DigestData(d digest.Digest, fname string) digest.Data {
	return digest.NewData(ds.Source(d, fname), d)
}

func (ds dataSource) Source(d digest.Digest, fname string) digest.Source {
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
		src := s.dataSource.cache.Source(s.d, s.fname)
		r, err := src.Open(ctx)
		if err == nil {
			return r, nil
		}
		// fallback
	}
	if s.dataSource.client != nil {
		buf, err := s.dataSource.client.Get(ctx, s.d, s.fname)
		if err != nil {
			return nil, err
		}
		return io.NopCloser(bytes.NewReader(buf)), nil
	}
	// no reapi configured. use local file?
	f, err := os.Open(s.fname)
	return f, err
}

func (s source) String() string {
	return fmt.Sprintf("dataSource:%s", s.fname)
}

func rotateFiles(ctx context.Context, fname string) {
	for i := 8; i >= 0; i-- {
		err := os.Rename(
			fmt.Sprintf("%s.%d", fname, i),
			fmt.Sprintf("%s.%d", fname, i+1))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			clog.Warningf(ctx, "rotate %s %d->%d failed: %v", fname, i, i+1, err)
		}
	}
	err := os.Rename(fname, fmt.Sprintf("%s.0", fname))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		clog.Warningf(ctx, "rotate %s ->0 failed: %v", fname, err)
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
		return func(context.Context, string) bool { return false }, nil
	default:
		return nil, fmt.Errorf("unknown output local strategy: %q. should be full/greedy/minimum", c.outputLocalStrategy)
	}
}

type lastTargets struct {
	Targets []string `json:"targets,omitempty"`
}

func loadTargets(ctx context.Context, targetsFile string) ([]string, error) {
	buf, err := os.ReadFile(targetsFile)
	if err != nil {
		return nil, err
	}
	var last lastTargets
	err = json.Unmarshal(buf, &last)
	if err != nil {
		return nil, fmt.Errorf("parse error %s: %w", targetsFile, err)
	}
	return last.Targets, nil
}

func saveTargets(ctx context.Context, targetsFile string, targets []string) error {
	v := lastTargets{
		Targets: targets,
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

func checkTargets(ctx context.Context, lastTargetsFile string, targets []string) bool {
	lastTargets, err := loadTargets(ctx, lastTargetsFile)
	if err != nil {
		clog.Warningf(ctx, "checkTargets: %v", err)
		return false
	}
	if len(targets) != len(lastTargets) {
		return false
	}
	sort.Strings(targets)
	sort.Strings(lastTargets)
	for i := range targets {
		if targets[i] != lastTargets[i] {
			return false
		}
	}
	return true
}
