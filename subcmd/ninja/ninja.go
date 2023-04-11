// Copyright 2023 The Chromium Authors. All rights reserved.
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
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	log "github.com/golang/glog"
	"github.com/google/uuid"
	"github.com/maruel/subcommands"
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
	"infra/build/siso/reapi"
	"infra/build/siso/reapi/digest"
	"infra/build/siso/sync/semaphore"
	"infra/build/siso/toolsupport/ninjautil"
	"infra/build/siso/ui"
)

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

	dryRun     bool
	clobber    bool
	actionSalt string

	fname string

	cacheDir         string
	localCacheEnable bool
	cacheEnableRead  bool
	// cacheEnableWrite bool

	configRepoDir  string
	configFilename string

	outputLocalStrategy string

	depsLogFile string
	// depsLogBucket

	localexecLogFile string
	metricsJSON      string
	traceJSON        string
	buildPprof       string
	// uploadBuildPprof bool

	fsopt             *hashfs.Option
	reopt             *reapi.Option
	reCacheEnableRead bool
	// reCacheEnableWrite bool

	// enableCloudLogging bool
	// enableCPUProfiler bool
	// cloudProfilerServiceName string
	// enableCloudTrace bool
	traceThreshold     time.Duration
	traceSpanThreshold time.Duration
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
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		return 1
	}
	return 0
}

func (c *ninjaCmdRun) run(ctx context.Context) (err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer signals.HandleInterrupt(cancel)()

	projectID := c.reopt.UpdateProjectID(c.projectID)
	var credential cred.Cred
	if projectID != "" {
		credential, err = cred.New(ctx, c.authOpts)
		if err != nil {
			return err
		}
	}
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	buildID := uuid.New().String()
	// enable cloud logging

	if cmdver, err := version.GetStartupVersion(); err != nil {
		clog.Warningf(ctx, "cannot determine CIPD package version: %s", err)
	} else {
		clog.Infof(ctx, "CIPD package name: %s", cmdver.PackageName)
		clog.Infof(ctx, "CIPD instance ID: %s", cmdver.InstanceID)
	}

	clog.Infof(ctx, "build id: %q", buildID)
	clog.Infof(ctx, "project id: %q", projectID)
	clog.Infof(ctx, "commandline %q", os.Args)

	// enable cloud profiler
	// enable cloud trace
	// upload build pprof

	clog.Infof(ctx, "wd: %s", wd)
	if !filepath.IsAbs(c.configRepoDir) {
		c.configRepoDir = filepath.Join(wd, c.configRepoDir)
	}
	cfgrepos := map[string]fs.FS{
		"config":           os.DirFS(c.configRepoDir),
		"config_overrides": os.DirFS(filepath.Join(wd, ".siso_remote")),
	}
	err = os.Chdir(c.dir)
	if err != nil {
		return err
	}
	clog.Infof(ctx, "change dir to %s", c.dir)
	logSymlink(ctx)

	if c.configFilename == "" {
		return err
	}
	gnargs, err := os.ReadFile("args.gn")
	if err != nil {
		clog.Warningf(ctx, "no args.gn: %v", err)
	}
	flags := make(map[string]string)
	c.Flags.Visit(func(f *flag.Flag) {
		name := f.Name
		if name == "C" {
			name = "dir"
		}
		flags[name] = f.Value.String()
	})
	flags["targets"] = strings.Join(c.Flags.Args(), " ")

	config, err := buildconfig.New(ctx, c.configFilename, flags, cfgrepos)
	if err != nil {
		return err
	}
	config.Metadata.KV["args.gn"] = string(gnargs)

	var spin ui.Spinner
	// depsLogBucket

	var localDepsLog *ninjautil.DepsLog
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		depsLog, err := ninjautil.NewDepsLog(ctx, c.depsLogFile)
		if err != nil {
			clog.Warningf(ctx, "failed to load deps log: %v", err)
		} else {
			localDepsLog = depsLog
		}
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
		ui.PrintLines(fmt.Sprintf("reapi instance: %s\n", c.reopt.Instance))
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
	var localexecLogWriter io.Writer
	if c.localexecLogFile != "" {
		f, err := os.Create(c.localexecLogFile)
		if err != nil {
			return err
		}
		defer func() {
			clog.Infof(ctx, "close localexec log")
			cerr := f.Close()
			if cerr != nil {
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
			if cerr != nil {
				err = cerr
			}
		}()
		metricsJSONWriter = f
	}
	var actionSaltBytes []byte
	if c.actionSalt != "" {
		actionSaltBytes = []byte(c.actionSalt)
	}
	if filepath.IsAbs(c.dir) {
		// recipe may use absolute path for -C.
		rdir, err := filepath.Rel(wd, c.dir)
		if err != nil {
			return err
		}
		if rdir == ".." || strings.HasPrefix(filepath.ToSlash(rdir), "../") {
			return fmt.Errorf("dir %q is out of exec root %q", c.dir, wd)
		}
		c.dir = rdir
	}
	buildPath := build.NewPath(wd, c.dir)
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
		ID:                 buildID,
		ProjectID:          projectID,
		Metadata:           config.Metadata,
		Path:               buildPath,
		HashFS:             hashFS,
		REAPIClient:        client,
		RECacheEnableRead:  c.reCacheEnableRead,
		ActionSalt:         actionSaltBytes,
		OutputLocal:        outputLocal,
		Cache:              cache,
		LocalexecLogWriter: localexecLogWriter,
		MetricsJSONWriter:  metricsJSONWriter,
		TraceJSON:          c.traceJSON,
		Pprof:              c.buildPprof,
		Clobber:            c.clobber,
		DryRun:             c.dryRun,
	}
	for {
		clog.Infof(ctx, "build starts")
		graph, err := ninjabuild.NewGraph(ctx, c.fname, config, buildPath, hashFS, localDepsLog)
		if err != nil {
			return err
		}
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
		return err
	}
}

func (c *ninjaCmdRun) init() {
	c.Flags.StringVar(&c.dir, "C", ".", "ninja running directory")
	c.Flags.StringVar(&c.configName, "config", "", "config name passed to starlark")
	c.Flags.StringVar(&c.projectID, "project", os.Getenv("SISO_PROJECT"), "cloud project ID. can set by $SISO_PROJECT")

	c.Flags.BoolVar(&c.dryRun, "n", false, "dry run")
	c.Flags.BoolVar(&c.clobber, "clobber", false, "clobber build")
	c.Flags.StringVar(&c.actionSalt, "action_salt", "", "action salt")

	c.Flags.StringVar(&c.fname, "f", "build.ninja", "input build manifet filename (relative to -C)")

	c.Flags.StringVar(&c.cacheDir, "cache_dir", defaultCacheDir(), "cache directory")
	c.Flags.BoolVar(&c.localCacheEnable, "local_cache_enable", false, "local cache enable")
	c.Flags.BoolVar(&c.cacheEnableRead, "cache_enable_read", true, "cache enable read")

	c.Flags.StringVar(&c.configRepoDir, "config_repo_dir", "build/config/siso", "config repo directory (relative to exec root)")
	c.Flags.StringVar(&c.configFilename, "load", "@config//main.star", "config filename (@config// is --config_repo_dir)")
	c.Flags.StringVar(&c.outputLocalStrategy, "output_local_strategy", "full", `strategy for output_local. "full": download all outputs. "greedy": downloads most outputs except intermediate objs. "minimum": downloads as few as possible`)
	c.Flags.StringVar(&c.depsLogFile, "deps_log", ".siso_deps", "deps log filename (relative to -C)")
	c.Flags.StringVar(&c.localexecLogFile, "localexec_log", ".siso_localexec", "localexec log filename (relative to -C")
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
}

func defaultCacheDir() string {
	d, err := os.UserCacheDir()
	if err != nil {
		log.Warningf("Failed to get user cache dir: %v", err)
		return ""
	}
	return filepath.Join(d, "siso")
}

func doBuild(ctx context.Context, graph *ninjabuild.Graph, bopts build.Options, args ...string) error {
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
		mfb.Close(ctx)
		return err
	}
	// TODO(b/266518906): upload manifest

	b, err := build.New(ctx, graph, bopts)
	if err != nil {
		return err
	}
	defer b.Close(ctx)
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
		dd := s.dataSource.cache.DigestData(s.d, s.fname)
		r, err := dd.Open(ctx)
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
