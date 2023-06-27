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
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"cloud.google.com/go/logging"
	"cloud.google.com/go/profiler"
	log "github.com/golang/glog"
	"github.com/google/uuid"
	"github.com/maruel/subcommands"
	"go.chromium.org/luci/auth"
	"go.chromium.org/luci/cipd/version"
	"go.chromium.org/luci/common/cli"
	"go.chromium.org/luci/common/system/signals"
	"google.golang.org/api/option"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/grpc/grpclog"

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

// File name of ninja log.
const ninjaLogName = ".ninja_log"

// Cmd returns the Command for the `ninja` subcommand provided by this package.
func Cmd(authOpts auth.Options) *subcommands.Command {
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
	authOpts auth.Options

	// flag values
	dir        string
	configName string
	projectID  string

	batch      bool
	dryRun     bool
	clobber    bool
	actionSalt string

	ninjaJobs int
	fname     string

	cacheDir         string
	localCacheEnable bool
	cacheEnableRead  bool
	// cacheEnableWrite bool

	configRepoDir  string
	configFilename string

	outputLocalStrategy string

	depsLogFile string
	// depsLogBucket

	failureSummaryFile string
	outputLogFile      string
	localexecLogFile   string
	metricsJSON        string
	traceJSON          string
	buildPprof         string
	// uploadBuildPprof bool

	fsopt             *hashfs.Option
	reopt             *reapi.Option
	reCacheEnableRead bool
	// reCacheEnableWrite bool

	enableCloudLogging bool
	// enableCPUProfiler bool
	enableCloudProfiler      bool
	cloudProfilerServiceName string
	enableCloudTrace         bool
	traceThreshold           time.Duration
	traceSpanThreshold       time.Duration

	debugMode  debugMode
	adjustWarn string
}

// Run runs the `ninja` subcommand.
func (c *ninjaCmdRun) Run(a subcommands.Application, args []string, env subcommands.Env) int {
	ctx := cli.GetContext(a, c, env)
	err := c.run(ctx)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrLoginRequired):
			fmt.Fprintf(os.Stderr, "need to login: run `siso login`\n")
		default:
			msgPrefix := "Error:"
			suggest := fmt.Sprintf("see %s/%s for command output, or %s/siso.INFO", c.dir, c.outputLogFile, c.dir)
			if ui.IsTerminal() {
				msgPrefix = ui.SGR(ui.BackgroundRed, msgPrefix)
				suggest = ui.SGR(ui.Bold, suggest)
			}
			fmt.Fprintf(os.Stderr, "%s: %s\n %v\n", msgPrefix, suggest, err)
		}
		return 1
	}
	return 0
}

func (c *ninjaCmdRun) run(ctx context.Context) (err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer signals.HandleInterrupt(cancel)()

	if c.debugMode.check() {
		return nil
	}

	if c.ninjaJobs >= 0 {
		fmt.Fprintf(os.Stderr, "-j is specified. but not supported. b/288829511\n")
	}

	if c.adjustWarn != "" {
		fmt.Fprintf(os.Stderr, "-w is specified. but not supported. b/288807840\n")
	}

	projectID := c.reopt.UpdateProjectID(c.projectID)
	var credential cred.Cred
	if projectID != "" {
		credential, err = cred.New(ctx, c.authOpts)
		if err != nil {
			return err
		}
	}
	// don't use $PWD for current directory
	// to avoid symlink issue. b/286779149
	pwd := os.Getenv("PWD")
	os.Unsetenv("PWD")
	execRoot, err := os.Getwd()
	if pwd != "" {
		os.Setenv("PWD", pwd)
	}
	if err != nil {
		return err
	}
	clog.Infof(ctx, "wd: %s", execRoot)
	if !filepath.IsAbs(c.configRepoDir) {
		execRoot, err = detectExecRoot(ctx, execRoot, c.configRepoDir)
		if err != nil {
			return err
		}
		c.configRepoDir = filepath.Join(execRoot, c.configRepoDir)
	}
	clog.Infof(ctx, "exec_root: %s", execRoot)

	buildID := uuid.New().String()
	if c.enableCloudLogging {
		log.Infof("enable cloud logging project=%s id=%s", projectID, buildID)

		spin := ui.Default.NewSpinner()
		spin.Start("init cloud logging to %s...", projectID)
		// log_id: "siso.log" and "siso.step"
		// use generic_task resource
		// https://cloud.google.com/logging/docs/api/v2/resource-list
		// https://cloud.google.com/monitoring/api/resources#tag_generic_task
		client, err := logging.NewClient(ctx, projectID, credential.ClientOptions()...)
		if err != nil {
			return err
		}
		hostname, err := os.Hostname()
		if err != nil {
			return err
		}
		logger, err := clog.New(ctx, client, "siso.log", "siso.step", &mrpb.MonitoredResource{
			Type: "generic_task",
			Labels: map[string]string{
				"project_id": projectID,
				"location":   hostname,
				"namespace":  execRoot,
				"job":        fmt.Sprintf("%q", os.Args),
				"task_id":    buildID,
			},
		})
		spin.Stop(err)
		fmt.Println(logger.URL())
		if err != nil {
			return err
		}
		defer logger.Close()
		ctx = clog.NewContext(ctx, logger)
		grpclog.SetLoggerV2(logger)
	}

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
	var traceExporter *trace.Exporter
	if c.enableCloudTrace {
		clog.Infof(ctx, "enable trace in %s [trace > %s]", projectID, c.traceThreshold)
		traceExporter, err = trace.NewExporter(ctx, trace.Options{
			ProjectID:     projectID,
			StepThreshold: c.traceThreshold,
			SpanThreshold: c.traceSpanThreshold,
			ClientOptions: append([]option.ClientOption{}, credential.ClientOptions()...),
		})
		if err != nil {
			clog.Errorf(ctx, "failed to start trace exporter: %v", err)
		}
		defer traceExporter.Close(ctx)
	}
	// upload build pprof

	cfgrepos := map[string]fs.FS{
		"config":           os.DirFS(c.configRepoDir),
		"config_overrides": os.DirFS(filepath.Join(execRoot, ".siso_remote")),
	}
	err = os.Chdir(c.dir)
	if err != nil {
		return err
	}
	clog.Infof(ctx, "change dir to %s", c.dir)
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	// recalculate dir as relative to exec_root.
	// recipe may use absolute path for -C.
	rdir, err := filepath.Rel(execRoot, cwd)
	if err != nil {
		return err
	}
	if !filepath.IsLocal(rdir) {
		return fmt.Errorf("dir %q is out of exec root %q", cwd, execRoot)
	}
	c.dir = rdir
	clog.Infof(ctx, "working_directory in exec_root: %s", c.dir)
	logSymlink(ctx)

	if c.configFilename == "" {
		return err
	}
	flags := make(map[string]string)
	c.Flags.Visit(func(f *flag.Flag) {
		name := f.Name
		if name == "C" {
			name = "dir"
		}
		flags[name] = f.Value.String()
	})
	targets := c.Flags.Args()
	flags["targets"] = strings.Join(targets, " ")
	lastTargetsFile := ".siso_last_targets"
	sameTargets := checkTargets(ctx, lastTargetsFile, targets)

	config, err := buildconfig.New(ctx, c.configFilename, flags, cfgrepos)
	if err != nil {
		return err
	}

	if gnArgs, err := os.ReadFile("args.gn"); err == nil {
		config.Metadata.Set("args.gn", string(gnArgs))
	} else {
		clog.Warningf(ctx, "no args.gn: %v", err)
	}

	spin := ui.Default.NewSpinner()
	// depsLogBucket

	var localDepsLog *ninjautil.DepsLog
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := os.MkdirAll(filepath.Dir(c.depsLogFile), 0755)
		if err != nil {
			clog.Warningf(ctx, "failed to mkdir for deps log: %v", err)
			return
		}
		depsLog, err := ninjautil.NewDepsLog(ctx, c.depsLogFile)
		if err != nil {
			clog.Warningf(ctx, "failed to load deps log: %v", err)
			return
		}
		localDepsLog = depsLog
	}()

	if !c.localCacheEnable {
		c.cacheDir = ""
	}
	var cacheStore build.CacheStore
	cacheStore, err = build.NewLocalCache(c.cacheDir)
	if err != nil {
		clog.Warningf(ctx, "no local cache enabled: %v", err)
	}
	var client *reapi.Client
	if c.reopt.IsValid() {
		ui.Default.PrintLines(fmt.Sprintf("reapi instance: %s\n", c.reopt.Instance))
		client, err = reapi.New(ctx, credential, *c.reopt)
		if err != nil {
			return err
		}
		cacheStore = client.CacheStore()
	}
	cache, err := build.NewCache(ctx, build.CacheOptions{
		Store:      cacheStore,
		EnableRead: c.cacheEnableRead,
	})
	if err != nil {
		clog.Warningf(ctx, "no cache enabled: %v", err)
	}
	spin.Start("loading fs state...")
	c.fsopt.DataSource = dataSource{
		cache:  cacheStore,
		client: client,
	}
	hashFS, err := hashfs.New(ctx, *c.fsopt)
	spin.Stop(err)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil && c.batch {
			return
		}
		clog.Infof(ctx, "save targets to %s...", lastTargetsFile)
		serr := saveTargets(ctx, lastTargetsFile, targets)
		if serr != nil {
			clog.Warningf(ctx, "failed to save failed targets: %v", serr)
		}
	}()
	defer func() {
		err := hashFS.Close(ctx)
		if err != nil {
			clog.Errorf(ctx, "close hashfs: %v", err)
		}
	}()

	clog.Infof(ctx, "sameTargets:%t hashfs clean:%t", sameTargets, hashFS.IsClean())
	if !c.clobber && sameTargets && hashFS.IsClean() {
		// TODO: better to check digest of .siso_fs_state?
		ui.Default.PrintLines("Everything is up-to-date. Nothing to do.\n")
		return nil
	}
	os.Remove(lastTargetsFile)

	var failureSummaryWriter io.Writer
	if c.failureSummaryFile != "" {
		f, err := os.Create(c.failureSummaryFile)
		if err != nil {
			return err
		}
		defer func() {
			clog.Infof(ctx, "close failure summary")
			cerr := f.Close()
			if err == nil {
				err = cerr
			}
		}()
		failureSummaryWriter = f
	}

	var outputLogWriter io.Writer
	if c.outputLogFile != "" {
		f, err := os.Create(c.outputLogFile)
		if err != nil {
			return err
		}
		defer func() {
			clog.Infof(ctx, "close output log")
			cerr := f.Close()
			if err == nil {
				err = cerr
			}
		}()
		outputLogWriter = f
	}
	var localexecLogWriter io.Writer
	if c.localexecLogFile != "" {
		f, err := os.Create(c.localexecLogFile)
		if err != nil {
			return err
		}
		defer func() {
			clog.Infof(ctx, "close localexec log")
			cerr := f.Close()
			if err == nil {
				err = cerr
			}
		}()
		localexecLogWriter = f
	}
	var metricsJSONWriter io.Writer
	if c.metricsJSON != "" {
		rotateFiles(ctx, c.metricsJSON)
		f, err := os.Create(c.metricsJSON)
		if err != nil {
			return err
		}
		defer func() {
			clog.Infof(ctx, "close metrics json")
			cerr := f.Close()
			if err == nil {
				err = cerr
			}
		}()
		metricsJSONWriter = f
	}
	// TODO(b/288826281): produce ninja log in the valid format.
	ninjaLogWriter, err := os.OpenFile(ninjaLogName, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer func() {
		clog.Infof(ctx, "close .ninja_log")
		cerr := ninjaLogWriter.Close()
		if err == nil {
			err = cerr
		}
	}()
	var actionSaltBytes []byte
	if c.actionSalt != "" {
		actionSaltBytes = []byte(c.actionSalt)
	}
	buildPath := build.NewPath(execRoot, c.dir)
	if c.traceJSON != "" {
		rotateFiles(ctx, c.traceJSON)
	}
	var outputLocal func(context.Context, string) bool
	switch c.outputLocalStrategy {
	case "full":
		outputLocal = func(context.Context, string) bool { return true }
	case "greedy":
		outputLocal = func(ctx context.Context, fname string) bool {
			// Note: d. wil be downloaded to get deps anyway,
			// but will not be written to disk.
			switch filepath.Ext(fname) {
			case ".o", ".obj", ".a", ".d", ".stamp":
				return false
			}
			return true
		}
	case "minimum":
		outputLocal = func(context.Context, string) bool { return false }
	default:
		return fmt.Errorf("unknown output local strategy:%q. should be full/greedy/minimum", c.outputLocalStrategy)
	}
	wg.Wait() // wait for localDepsLog loading
	if localDepsLog != nil {
		defer localDepsLog.Close()
	}
	bopts := build.Options{
		ID:                   buildID,
		ProjectID:            projectID,
		Metadata:             config.Metadata,
		Path:                 buildPath,
		HashFS:               hashFS,
		REAPIClient:          client,
		RECacheEnableRead:    c.reCacheEnableRead,
		ActionSalt:           actionSaltBytes,
		OutputLocal:          outputLocal,
		Cache:                cache,
		FailureSummaryWriter: failureSummaryWriter,
		OutputLogWriter:      outputLogWriter,
		LocalexecLogWriter:   localexecLogWriter,
		MetricsJSONWriter:    metricsJSONWriter,
		NinjaLogWriter:       ninjaLogWriter,
		TraceExporter:        traceExporter,
		TraceJSON:            c.traceJSON,
		Pprof:                c.buildPprof,
		Clobber:              c.clobber,
		DryRun:               c.dryRun,
		KeepRSP:              c.debugMode.Keeprsp,
	}
	const failedTargetsFile = ".siso_failed_targets"
	for {
		clog.Infof(ctx, "build starts")
		graph, err := ninjabuild.NewGraph(ctx, c.fname, config, buildPath, hashFS, localDepsLog)
		if err != nil {
			return err
		}
		if !c.batch && sameTargets && !c.clobber {
			failedTargets, err := loadTargets(ctx, failedTargetsFile)
			if err != nil {
				clog.Infof(ctx, "no failed targets: %v", err)
			} else {
				lbopts := bopts
				lbopts.REAPIClient = nil
				ui.Default.PrintLines(fmt.Sprintf("Building last failed targets: %s...\n", failedTargets))
				err = doBuild(ctx, graph, lbopts, failedTargets...)
				if errors.Is(err, build.ErrManifestModified) {
					clog.Infof(ctx, "%s modified. refresh hashfs...", c.fname)
					// need to refresh cached entries as `gn gen` updated files
					// but nnja manifest doesn't know what files are updated.
					err := hashFS.Refresh(ctx, buildPath.ExecRoot)
					if err != nil {
						return err
					}
					clog.Infof(ctx, "refresh hashfs done. build retry")
					continue
				}
				if err != nil {
					return err
				}
				os.Remove(failedTargetsFile)
				ui.Default.PrintLines(fmt.Sprintf(" %s: %s\n", ui.SGR(ui.Green, "last failed targets fixed"), failedTargets))
				continue
			}
		}
		os.Remove(failedTargetsFile)
		err = doBuild(ctx, graph, bopts, c.Flags.Args()...)
		if errors.Is(err, build.ErrManifestModified) {
			clog.Infof(ctx, "%s modified. refresh hashfs...", c.fname)
			// need to refresh cached entries as `gn gen` updated files
			// but nnja manifest doesn't know what files are updated.
			err := hashFS.Refresh(ctx, buildPath.ExecRoot)
			if err != nil {
				return err
			}
			clog.Infof(ctx, "refresh hashfs done. build retry")
			continue
		}
		clog.Infof(ctx, "build finished: %v", err)
		if err != nil && !c.batch {
			var stepError build.StepError
			if errors.As(err, &stepError) {
				clog.Infof(ctx, "record failed targets: %q", stepError.Target)
				serr := saveTargets(ctx, failedTargetsFile, []string{stepError.Target})
				if serr != nil {
					clog.Warningf(ctx, "failed to save failed targets: %v", serr)
				}
			}
		}
		return err
	}
}

func (c *ninjaCmdRun) init() {
	c.Flags.StringVar(&c.dir, "C", ".", "ninja running directory")
	c.Flags.StringVar(&c.configName, "config", "", "config name passed to starlark")
	c.Flags.StringVar(&c.projectID, "project", os.Getenv("SISO_PROJECT"), "cloud project ID. can set by $SISO_PROJECT")

	c.Flags.BoolVar(&c.batch, "batch", !ui.IsTerminal(), "batch mode. prefer thoughput over low latency for build failures.")
	c.Flags.BoolVar(&c.dryRun, "n", false, "dry run")
	c.Flags.BoolVar(&c.clobber, "clobber", false, "clobber build")
	c.Flags.StringVar(&c.actionSalt, "action_salt", "", "action salt")

	c.Flags.IntVar(&c.ninjaJobs, "j", -1, "run N jobs in parallel (0 means infinity). not supported b/288829511")
	c.Flags.StringVar(&c.fname, "f", "build.ninja", "input build manifet filename (relative to -C)")

	c.Flags.StringVar(&c.cacheDir, "cache_dir", defaultCacheDir(), "cache directory")
	c.Flags.BoolVar(&c.localCacheEnable, "local_cache_enable", false, "local cache enable")
	c.Flags.BoolVar(&c.cacheEnableRead, "cache_enable_read", true, "cache enable read")

	c.Flags.StringVar(&c.configRepoDir, "config_repo_dir", "build/config/siso", "config repo directory (relative to exec root)")
	c.Flags.StringVar(&c.configFilename, "load", "@config//main.star", "config filename (@config// is --config_repo_dir)")
	c.Flags.StringVar(&c.outputLocalStrategy, "output_local_strategy", "full", `strategy for output_local. "full": download all outputs. "greedy": downloads most outputs except intermediate objs. "minimum": downloads as few as possible`)
	c.Flags.StringVar(&c.depsLogFile, "deps_log", ".siso_deps", "deps log filename (relative to -C)")
	c.Flags.StringVar(&c.failureSummaryFile, "failure_summary", "", "filename for failure summary (relative to -C)")
	c.Flags.StringVar(&c.outputLogFile, "output_log", "siso_output", "output log filename (relative to -C")
	c.Flags.StringVar(&c.localexecLogFile, "localexec_log", "siso_localexec", "localexec log filename (relative to -C")
	c.Flags.StringVar(&c.metricsJSON, "metrics_json", "siso_metrics.json", "metrics JSON filename (relative to -C)")
	c.Flags.StringVar(&c.traceJSON, "trace_json", "siso_trace.json", "trace JSON filename (relative to -C)")
	c.Flags.StringVar(&c.buildPprof, "build_pprof", "siso_build.pprof", "build pprof filename (relative to -C)")

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

	c.Flags.DurationVar(&c.traceThreshold, "trace_threshold", 1*time.Minute, "threshold for trace record")
	c.Flags.DurationVar(&c.traceSpanThreshold, "trace_span_threshold", 100*time.Millisecond, "theshold for trace span record")

	c.Flags.BoolVar(&c.enableCloudLogging, "enable_cloud_logging", false, "enable cloud logging")
	c.Flags.BoolVar(&c.enableCloudProfiler, "enable_cloud_profiler", false, "enable cloud profiler")
	c.Flags.StringVar(&c.cloudProfilerServiceName, "cloud_profiler_service_name", "siso", "cloud profiler service name")
	c.Flags.BoolVar(&c.enableCloudTrace, "enable_cloud_trace", false, "enable cloud trace")

	c.Flags.Var(&c.debugMode, "d", "enable debugging (use '-d list' to list modes)")
	c.Flags.StringVar(&c.adjustWarn, "w", "", "adjust warnings. not supported b/288807840")
}

func defaultCacheDir() string {
	d, err := os.UserCacheDir()
	if err != nil {
		log.Warningf("Failed to get user cache dir: %v", err)
		return ""
	}
	return filepath.Join(d, "siso")
}

func doBuild(ctx context.Context, graph *ninjabuild.Graph, bopts build.Options, args ...string) (err error) {
	clog.Infof(ctx, "rebuild manifest")
	mfbopts := bopts
	mfbopts.Clobber = false
	mfbopts.RebuildManifest = graph.Filename()
	mfb, err := build.New(ctx, graph, mfbopts)
	if err != nil {
		return err
	}
	err = mfb.Build(ctx, "rebuild manifest", graph.Filename())
	if err != nil {
		return err
	}
	// TODO(b/266518906): upload manifest

	b, err := build.New(ctx, graph, bopts)
	if err != nil {
		return err
	}
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
			t.servAvg = ts.Avg()
			t.servBuckets = ts.Buckets
			semaTraces[name] = t
		case ts.Name == "rbe:queue":
			name := "rbe:sched"
			t := semaTraces[name]
			t.name = name
			t.n = ts.N
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
		var semaNames []string
		for key := range semaTraces {
			semaNames = append(semaNames, key)
		}
		sort.Strings(semaNames)
		tw := tabwriter.NewWriter(os.Stdout, 10, 8, 1, ' ', tabwriter.AlignRight)
		fmt.Fprintf(tw, "resource/capa\tused\twait-avg\t|   s m |\tserv-avg\t|   s m |\t\n")
		for _, key := range semaNames {
			t := semaTraces[key]
			s, _ := semaphore.Lookup(t.name)
			c := "nil"
			if s != nil {
				c = strconv.Itoa(s.Capacity())
			}
			fmt.Fprintf(tw, "%s/%s\t%d\t%s\t%s\t%s\t%s\t\n", t.name, c, t.n, t.waitAvg.Round(time.Millisecond), histogram(t.waitBuckets), t.servAvg.Round(time.Millisecond), histogram(t.servBuckets))
		}
		tw.Flush()
	}
	if bopts.REAPIClient == nil {
		return err
	}

	// TODO(b/266518906): wait for completion of uploading manifest
	return err
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
	n                        int
	waitAvg, servAvg         time.Duration
	waitBuckets, servBuckets [7]int
}

func logSymlink(ctx context.Context) {
	var logDirs []string
	logDirFlag := flag.Lookup("log_dir")
	if logDirFlag != nil && logDirFlag.Value.String() != "" {
		logDirs = append(logDirs, logDirFlag.Value.String())
	}
	logDirs = append(logDirs, os.TempDir())
	logFilename := "siso.INFO"
	if runtime.GOOS == "windows" {
		logFilename = "siso.exe.INFO"
	}
	rotateFiles(ctx, logFilename)
	for _, dir := range logDirs {
		target, err := os.Readlink(filepath.Join(dir, logFilename))
		if err != nil {
			if log.V(1) {
				clog.Infof(ctx, "log file not found in %s: %v", dir, err)
			}
			continue
		}
		os.Remove(logFilename)
		logFname := filepath.Join(dir, target)
		err = os.Symlink(logFname, logFilename)
		if err != nil {
			clog.Warningf(ctx, "failed to create %s: %v", logFilename, err)
			return
		}
		clog.Infof(ctx, "logfile: %s", logFname)
		return
	}
	clog.Warningf(ctx, "failed to find %s in %q", logFilename, logDirs)
}

type dataSource struct {
	cache  build.CacheStore
	client *reapi.Client
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
