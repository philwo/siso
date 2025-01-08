// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/logging"
	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	log "github.com/golang/glog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"infra/build/siso/build/metadata"
	"infra/build/siso/execute"
	"infra/build/siso/execute/localexec"
	"infra/build/siso/execute/remoteexec"
	"infra/build/siso/execute/reproxyexec"
	"infra/build/siso/hashfs"
	"infra/build/siso/hashfs/osfs"
	"infra/build/siso/o11y/clog"
	"infra/build/siso/o11y/iometrics"
	"infra/build/siso/o11y/monitoring"
	sisopprof "infra/build/siso/o11y/pprof"
	"infra/build/siso/o11y/resultstore"
	"infra/build/siso/o11y/trace"
	"infra/build/siso/reapi"
	"infra/build/siso/reapi/digest"
	"infra/build/siso/reapi/merkletree"
	"infra/build/siso/runtimex"
	"infra/build/siso/scandeps"
	"infra/build/siso/sync/semaphore"
	"infra/build/siso/toolsupport/ninjautil"
	"infra/build/siso/ui"
)

// logging labels's key.
const (
	logLabelKeyID        = "id"
	logLabelKeyBacktrace = "backtrace"
)

// chromium recipe module expects this string.
const ninjaNoWorkToDo = "ninja: no work to do.\n"

// OutputLocalFunc is a function to determine the file should be downloaded or not.
type OutputLocalFunc func(context.Context, string) bool

// Options is builder options.
type Options struct {
	JobID     string
	ID        string
	StartTime time.Time

	Metadata          metadata.Metadata
	ProjectID         string
	Path              *Path
	HashFS            *hashfs.HashFS
	REAPIClient       *reapi.Client
	RECacheEnableRead bool
	// TODO(b/266518906): enable RECacheEnableWrite option for read-only client.
	// RECacheEnableWrite bool
	ReproxyAddr string
	ActionSalt  []byte

	OutputLocal          OutputLocalFunc
	Cache                *Cache
	FailureSummaryWriter io.Writer
	FailedCommandsWriter io.Writer
	OutputLogWriter      io.Writer
	ExplainWriter        io.Writer
	LocalexecLogWriter   io.Writer
	MetricsJSONWriter    io.Writer
	NinjaLogWriter       io.Writer
	TraceJSON            string
	Pprof                string
	TraceExporter        *trace.Exporter
	PprofUploader        *sisopprof.Uploader
	ResultstoreUploader  *resultstore.Uploader

	// Clobber forces to rebuild ignoring existing generated files.
	Clobber bool

	// Build inputs of targets, but not build targets itself.
	Prepare bool

	// Verbose shows all command lines while building rather than step description.
	Verbose bool

	// VerboseFailures shows failed command lines.
	VerboseFailures bool

	// DryRun just prints the command to build, but does nothing.
	DryRun bool

	// allow failures at most FailuresAllowed.
	FailuresAllowed int

	// don't delete @response files on success
	KeepRSP bool

	// RebuildManifest is a build manifest filename (i.e. build.ninja)
	// when rebuilding manifest.
	// empty for normal build.
	RebuildManifest string

	// Limits specifies resource limits.
	Limits Limits
}

// Builder is a builder.
type Builder struct {
	jobID string // correlated invocations id.
	// build session id, tool invocation id.
	id        string
	projectID string
	metadata  metadata.Metadata

	progress progress

	// path system used in the build.
	path   *Path
	hashFS *hashfs.HashFS

	// arg table to intern command line args of steps.
	argTab symtab

	// start is the time at the build starts.
	start time.Time
	graph Graph
	plan  *plan
	stats *stats

	// record phony targets state.
	phony sync.Map

	stepSema *semaphore.Semaphore

	preprocSema *semaphore.Semaphore

	// for subtree: dir -> *subtree
	trees sync.Map

	scanDepsSema *semaphore.Semaphore
	scanDeps     *scandeps.ScanDeps

	localSema *semaphore.Semaphore
	poolSemas map[string]*semaphore.Semaphore
	localExec localexec.LocalExec

	rewrapSema *semaphore.Semaphore

	fastLocalSema *semaphore.Semaphore

	remoteSema        *semaphore.Semaphore
	remoteExec        *remoteexec.RemoteExec
	reCacheEnableRead bool
	// TODO(b/266518906): enable reCacheEnableWrite option for read-only client.
	// reCacheEnableWrite bool
	reapiclient *reapi.Client

	reproxySema *semaphore.Semaphore
	reproxyExec *reproxyexec.REProxyExec

	actionSalt []byte

	outputLocal OutputLocalFunc

	cacheSema *semaphore.Semaphore
	cache     *Cache

	explainWriter        io.Writer
	failureSummaryWriter io.Writer
	failedCommandsWriter io.Writer
	localexecLogWriter   io.Writer
	metricsJSONWriter    io.Writer
	ninjaLogWriter       io.Writer
	outputLogWriter      io.Writer
	traceExporter        *trace.Exporter
	traceEvents          *traceEvents
	traceStats           *traceStats
	tracePprof           *tracePprof
	pprofUploader        *sisopprof.Uploader
	resultstoreUploader  *resultstore.Uploader

	clobber         bool
	prepare         bool
	verbose         bool
	verboseFailures bool
	dryRun          bool

	failuresAllowed int

	// ninja debug modes
	keepRSP bool

	rebuildManifest string
}

// New creates new builder.
func New(ctx context.Context, graph Graph, opts Options) (*Builder, error) {
	logger := clog.FromContext(ctx)
	if logger != nil {
		logger.Formatter = logFormat
	}
	start := opts.StartTime
	if start.IsZero() {
		start = time.Now()
	}
	ew := opts.ExplainWriter
	if ew == nil {
		ew = io.Discard
	}
	lelw := opts.LocalexecLogWriter
	if lelw == nil {
		lelw = io.Discard
	}
	mw := opts.MetricsJSONWriter
	if mw == nil {
		mw = io.Discard
	}
	nw := opts.NinjaLogWriter
	if nw == nil {
		nw = io.Discard
	}

	if err := opts.Path.Check(); err != nil {
		return nil, err
	}
	if opts.HashFS == nil {
		return nil, fmt.Errorf("hash fs must be set")
	}
	var le localexec.LocalExec
	var re *remoteexec.RemoteExec
	var pe *reproxyexec.REProxyExec
	if opts.REAPIClient != nil {
		logger.Infof("enable built-in remote exec")
		re = remoteexec.New(ctx, opts.REAPIClient)
	} else {
		logger.Infof("disable built-in remote exec")
	}
	pe = reproxyexec.New(ctx, opts.ReproxyAddr)
	if pe.Enabled() {
		logger.Infof("enable reclient integration: addr=%s", opts.ReproxyAddr)
	} else {
		logger.Infof("disable reclient integration")
	}
	experiments.ShowOnce()
	numCPU := runtimex.NumCPU()
	if (opts.Limits == Limits{}) {
		opts.Limits = DefaultLimits(ctx)
	}
	// On many cores machine, it would hit default max thread limit = 10000.
	// Usually, it would require 1/3 of stepLimit threads (cache miss case?).
	// For safe, sets 1/2 of stepLimit for max threads. b/325565625
	maxThreads := opts.Limits.Step / 2
	if maxThreads > 10000 {
		debug.SetMaxThreads(maxThreads)
	} else {
		maxThreads = 10000
	}
	logger.Infof("numcpu=%d threads:%d - limits=%#v", numCPU, maxThreads, opts.Limits)
	logger.Infof("correlated_invocations_id: %s", opts.JobID)
	logger.Infof("tool_invocation_id: %s", opts.ID)

	var fastLocalSema *semaphore.Semaphore
	if opts.Limits.FastLocal > 0 {
		fastLocalSema = semaphore.New("fastlocal", opts.Limits.FastLocal)
	}
	return &Builder{
		jobID:     opts.JobID,
		id:        opts.ID,
		projectID: opts.ProjectID,
		metadata:  opts.Metadata,

		path:              opts.Path,
		hashFS:            opts.HashFS,
		start:             start,
		graph:             graph,
		stepSema:          semaphore.New("step", opts.Limits.Step),
		preprocSema:       semaphore.New("preproc", opts.Limits.Preproc),
		scanDepsSema:      semaphore.New("scandeps", opts.Limits.ScanDeps),
		scanDeps:          scandeps.New(opts.HashFS, graph.InputDeps(ctx)),
		localSema:         semaphore.New("localexec", opts.Limits.Local),
		localExec:         le,
		rewrapSema:        semaphore.New("rewrap", opts.Limits.REWrap),
		fastLocalSema:     fastLocalSema,
		remoteSema:        semaphore.New("remoteexec", opts.Limits.Remote),
		remoteExec:        re,
		reCacheEnableRead: opts.RECacheEnableRead,
		// reCacheEnableWrite: opts.RECacheEnableWrite,
		reproxyExec: pe,
		reproxySema: semaphore.New("reproxyexec", opts.Limits.Remote),
		actionSalt:  opts.ActionSalt,
		reapiclient: opts.REAPIClient,

		outputLocal:          opts.OutputLocal,
		cacheSema:            semaphore.New("cache", opts.Limits.Cache),
		cache:                opts.Cache,
		failureSummaryWriter: opts.FailureSummaryWriter,
		failedCommandsWriter: opts.FailedCommandsWriter,
		outputLogWriter:      opts.OutputLogWriter,
		explainWriter:        ew,
		localexecLogWriter:   lelw,
		metricsJSONWriter:    mw,
		ninjaLogWriter:       nw,
		traceExporter:        opts.TraceExporter,
		traceEvents:          newTraceEvents(opts.TraceJSON, opts.Metadata),
		traceStats:           newTraceStats(),
		tracePprof:           newTracePprof(opts.Pprof),
		pprofUploader:        opts.PprofUploader,
		resultstoreUploader:  opts.ResultstoreUploader,
		clobber:              opts.Clobber,
		prepare:              opts.Prepare,
		verbose:              opts.Verbose,
		verboseFailures:      opts.VerboseFailures,
		dryRun:               opts.DryRun,
		failuresAllowed:      opts.FailuresAllowed,
		keepRSP:              opts.KeepRSP,
		rebuildManifest:      opts.RebuildManifest,
	}, nil
}

// Close cleans up the builder.
func (b *Builder) Close() error {
	if b.reproxyExec == nil {
		return nil
	}
	return b.reproxyExec.Close()
}

// Stats returns stats of the builder.
func (b *Builder) Stats() Stats {
	return b.stats.stats()
}

// TraceStats returns trace stats of the builder.
func (b *Builder) TraceStats() []*TraceStat {
	return b.traceStats.get()
}

// ErrManifestModified is an error to indicate that manifest is modified.
var ErrManifestModified = errors.New("manifest modified")

type numBytes int64

var bytesUnit = map[int64]string{
	1 << 10: "KiB",
	1 << 20: "MiB",
	1 << 30: "GiB",
	1 << 40: "TiB",
}

func (b numBytes) String() string {
	var n []int64
	for k := range bytesUnit {
		n = append(n, k)
	}
	sort.Slice(n, func(i, j int) bool {
		return n[i] > n[j]
	})
	i := int64(b)
	for _, k := range n {
		if i >= k {
			return fmt.Sprintf("%.02f%s", float64(i)/float64(k), bytesUnit[k])
		}
	}
	return fmt.Sprintf("%dB", i)
}

// Build builds args with the name.
func (b *Builder) Build(ctx context.Context, name string, args ...string) (err error) {
	started := time.Now()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	defer func() {
		if r := recover(); r != nil {
			const size = 64 << 10
			buf := make([]byte, size)
			buf = buf[:runtime.Stack(buf, false)]
			loc := panicLocation(buf)
			clog.Errorf(ctx, "panic in build: %v\n%s", r, loc)
			clog.Warningf(ctx, "%s", buf)
			if err == nil {
				err = fmt.Errorf("panic in build: %v", r)
			}
		}
		clog.Infof(ctx, "build %v", err)
	}()

	if b.rebuildManifest == "" && b.reapiclient != nil {
		// upload build.ninja in background.
		// if build finished earilier, we'll cancel the uploading
		// since it would be better to finish build soon rather
		// than waiting for uploading build.ninja.
		go b.uploadBuildNinja(ctx)
	}

	// scheduling
	// TODO: run asynchronously?
	schedOpts := schedulerOption{
		NumTargets: b.graph.NumTargets(),
		Path:       b.path,
		HashFS:     b.hashFS,
		Prepare:    b.prepare,
	}
	sched := newScheduler(ctx, schedOpts)
	err = schedule(ctx, sched, b.graph, args...)
	if err != nil {
		if b.failureSummaryWriter != nil {
			fmt.Fprintf(b.failureSummaryWriter, "%v\n", err)
		}
		return err
	}
	b.plan = sched.plan
	b.stats = newStats(sched.total)

	stat := b.Stats()
	if stat.Total == 0 {
		clog.Infof(ctx, "nothing to build for %q", args)
		ui.Default.PrintLines(ninjaNoWorkToDo)
		return nil
	}

	var pools []string
	localLimit := b.localSema.Capacity()
	stepLimits := b.graph.StepLimits(ctx)
	for k := range stepLimits {
		pools = append(pools, k)
	}
	sort.Strings(pools)
	b.poolSemas = map[string]*semaphore.Semaphore{}
	for _, k := range pools {
		v := stepLimits[k]
		name := "pool=" + k
		if strings.HasSuffix(name, "_pool") {
			// short name for semaphore/resource name
			// e.g. build_toolchain_action_pool -> pool=action
			s := strings.Split(name, "_")
			name = "pool=" + s[len(s)-2]
		}
		// No matter what pools you specify, ninja will never run more concurrent jobs than the default parallelism,
		if v > localLimit {
			v = localLimit
		}
		b.poolSemas[k] = semaphore.New(name, v)
		clog.Infof(ctx, "limit %s -> %s=%d", k, name, v)
	}

	var mftime time.Time
	if b.rebuildManifest != "" {
		fi, err := b.hashFS.Stat(ctx, b.path.ExecRoot, filepath.Join(b.path.Dir, b.rebuildManifest))
		if err == nil {
			mftime = fi.ModTime()
			clog.Infof(ctx, "manifest %s: %s", b.rebuildManifest, mftime)
		}
	}
	defer func() {
		stat = b.Stats()
		if b.rebuildManifest != "" {
			fi, mferr := b.hashFS.Stat(ctx, b.path.ExecRoot, filepath.Join(b.path.Dir, b.rebuildManifest))
			if mferr != nil {
				clog.Warningf(ctx, "failed to stat %s: %v", b.rebuildManifest, mferr)
				return
			}
			if err != nil {
				return
			}
			clog.Infof(ctx, "rebuild manifest %#v %s: %s->%s: %s", stat, b.rebuildManifest, mftime, fi.ModTime(), time.Since(started))
			if fi.ModTime().After(mftime) || stat.Done != stat.Skipped {
				ui.Default.PrintLines(fmt.Sprintf("%6s Regenerating ninja files\n", ui.FormatDuration(time.Since(started))))
				err = ErrManifestModified
				return
			}
			return
		}
		clog.Infof(ctx, "build %s %s: %v", time.Since(started), time.Since(b.start), err)
		if stat.Skipped == stat.Total {
			ui.Default.PrintLines(ninjaNoWorkToDo)
			return
		}
		fsstat := b.hashFS.OS.IOMetrics.Stats()
		fsstatLine := fmt.Sprintf("fs: ops: %d(err:%d) / r:%d(err:%d) %s / w:%d(err:%d) %s\n",
			fsstat.Ops, fsstat.OpsErrs,
			fsstat.ROps, fsstat.RErrs, numBytes(fsstat.RBytes),
			fsstat.WOps, fsstat.WErrs, numBytes(fsstat.WBytes))
		var depsStatLine string
		var restatLine string
		if b.reapiclient != nil {
			// fastdeps / scandeps is only used in siso native mode.
			if stat.FastDepsSuccess != 0 || stat.FastDepsFailed != 0 || stat.ScanDepsFailed != 0 {
				depsStatLine = fmt.Sprintf("deps log:%d logErr:%d scanErr:%d\n",
					stat.FastDepsSuccess, stat.FastDepsFailed, stat.ScanDepsFailed)
			}
			restat := b.reapiclient.IOMetrics().Stats()
			restatLine = fmt.Sprintf("reapi: ops: %d(err:%d) / r:%d(err:%d) %s / w:%d(err:%d) %s\n",
				restat.Ops, restat.OpsErrs,
				restat.ROps, restat.RErrs, numBytes(restat.RBytes),
				restat.WOps, restat.WErrs, numBytes(restat.WBytes))
		}
		if !b.reproxyExec.Used() {
			// this stats will be shown by reproxy shutdown.
			msg := fmt.Sprintf("\nlocal:%d remote:%d cache:%d fallback:%d retry:%d skip:%d\n",
				stat.Local+stat.NoExec, stat.Remote, stat.CacheHit, stat.LocalFallback, stat.RemoteRetry, stat.Skipped) +
				depsStatLine +
				restatLine +
				fsstatLine
			ui.Default.PrintLines("\n", msg)
			if b.resultstoreUploader != nil {
				b.resultstoreUploader.AddBuildLog(msg + "\n")
			}
		} else {
			ui.Default.PrintLines("\n", "\n")
		}
	}()
	semas := []*semaphore.Semaphore{
		b.cache.sema,
		b.cacheSema,
		b.localSema,
		b.remoteSema,
		b.reproxySema,
		b.rewrapSema,
		b.stepSema,
		hashfs.FlushSemaphore,
		hashfs.ForgetMissingsSemaphore,
		osfs.LstatSemaphore,
		reapi.FileSemaphore,
		remoteexec.Semaphore,
	}
	b.traceEvents.Start(ctx, semas, []*iometrics.IOMetrics{
		b.hashFS.OS.IOMetrics,
		b.reapiclient.IOMetrics(),
		// TODO: cache iometrics?
	})
	defer b.traceEvents.Close(ctx)
	b.tracePprof.SetMetadata(b.metadata)
	b.pprofUploader.SetMetadata(ctx, b.metadata)
	defer func(ctx context.Context) {
		perr := b.tracePprof.Close(ctx)
		if perr != nil {
			clog.Warningf(ctx, "pprof close: %v", perr)
		}
		if b.pprofUploader != nil {
			perr := b.pprofUploader.Upload(ctx, b.tracePprof.p)
			if perr != nil {
				clog.Warningf(ctx, "upload pprof: %v", perr)
			} else {
				clog.Infof(ctx, "uploaded pprof")
			}
		} else {
			clog.Infof(ctx, "no pprof uploader")
		}

	}(ctx)
	pstat := b.plan.stats()
	b.progress.report("\nbuild start: Ready %d Pending %d", pstat.nready, pstat.npendings)
	clog.Infof(ctx, "build pendings=%d ready=%d", pstat.npendings, pstat.nready)
	b.progress.start(ctx, b)
	defer b.progress.stop(ctx)

	if b.clobber {
		fmt.Fprintf(b.explainWriter, "--clobber is specified\n")
	}

	// prepare all output directories for local process and reproxy,
	// to minimize mkdir operations.
	// if we create out dirs before each action concurrently,
	// need to check the dir many times and worry about race.
	err = b.prepareAllOutDirs(ctx)
	if err != nil {
		clog.Warningf(ctx, "failed to prepare all out dirs: %v", err)
		return err
	}

	var wg sync.WaitGroup
	var nerrs int
	var firstErr error
	var stuck bool
	errch := make(chan error, 1000)

loop:
	for {
		t := time.Now()
		ctx, done, err := b.stepSema.WaitAcquire(ctx)
		if err != nil {
			clog.Warningf(ctx, "wait acquire: %v", err)
			cancel()
			return err
		}
		dur := time.Since(t)
		if dur > 1*time.Millisecond {
			clog.Infof(ctx, "step sema wait %s", dur)
		}

		var step *Step
		var ok bool
		select {
		case step, ok = <-b.plan.q:
			if !ok {
				clog.Infof(ctx, "q is closed")
				done(nil)
				break loop
			}
		case err := <-errch:
			done(err)
			if err != nil {
				clog.Infof(ctx, "err from errch: %v", err)
				if !errors.Is(err, context.Canceled) {
					if firstErr == nil {
						firstErr = err
					}
					nerrs++
				}
			}
			numServs := b.stepSema.NumServs()
			hasReady := b.plan.hasReady()
			// no active steps and no ready steps?
			if !stuck {
				stuck = numServs == 0 && !hasReady
			}
			if log.V(1) {
				clog.Infof(ctx, "errs=%d numServs=%d hasReady=%t stuck=%t", nerrs, numServs, hasReady, stuck)
			}
			if nerrs >= b.failuresAllowed || stuck {
				clog.Infof(ctx, "unable to proceed nerrs=%d numServs=%d hasReady=%t stuck=%t", nerrs, numServs, hasReady, stuck)
				cancel()
				break loop
			}
			continue
		case <-ctx.Done():
			clog.Infof(ctx, "context done")
			done(context.Cause(ctx))
			cancel()
			b.plan.dump(ctx, b.graph)
			return context.Cause(ctx)
		}
		b.plan.pushReady()
		wg.Add(1)
		go func(step *Step) {
			defer wg.Done()
			var err error
			defer func() {
				// send errch after done is called. b/297301112
				select {
				case <-ctx.Done():
				case errch <- err:
				}
			}()
			defer func() {
				if r := recover(); r != nil {
					const size = 64 << 10
					buf := make([]byte, size)
					buf = buf[:runtime.Stack(buf, false)]
					var out string
					if outs := step.def.Outputs(ctx); len(outs) > 0 {
						out = outs[0]
					} else {
						out = fmt.Sprintf("%p", step)
					}
					loc := panicLocation(buf)
					clog.Errorf(ctx, "runStep panic: %v\nstep: %s\n%s", r, out, loc)
					clog.Warningf(ctx, "%s", buf)
					err = fmt.Errorf("panic: %v: %s", r, loc)
				}
			}()
			defer done(nil)

			err = b.runStep(ctx, step)
			select {
			case <-ctx.Done():
				return
			default:
			}
		}(step)
	}
	clog.Infof(ctx, "all pendings becomes ready")
	errdone := make(chan error)
	go func() {
		for e := range errch {
			if nerrs >= b.failuresAllowed {
				continue
			}
			if e != nil && !errors.Is(e, context.Canceled) {
				if firstErr == nil {
					firstErr = e
				}
				nerrs++
			}
		}
		// step error is already reported in run_step.
		// report errStuck if it coulddn't progress.
		// otherwise, report just number of errors.
		if nerrs == 0 {
			errdone <- nil
			return
		}
		if stuck {
			errdone <- fmt.Errorf("cannot make progress due to previous %d errors: %w", nerrs, firstErr)
			return
		}
		errdone <- fmt.Errorf("%d steps failed: %w", nerrs, firstErr)
	}()
	wg.Wait()
	close(errch)
	err = <-errdone
	if !b.verbose {
		// replace 2 progress lines.
		if err == nil {
			ui.Default.PrintLines(fmt.Sprintf("%s finished", name), "")
		} else {
			ui.Default.PrintLines(fmt.Sprintf("%s failed", name), "")
		}
	}
	// metrics for full build session, without step_id etc.
	var metrics StepMetric
	metrics.BuildID = b.id
	metrics.Duration = IntervalMetric(time.Since(b.start))
	metrics.Err = err != nil
	b.recordMetrics(ctx, metrics)
	clog.Infof(ctx, "%s finished: %v", name, err)
	if b.rebuildManifest == "" && !ui.IsTerminal() && b.failureSummaryWriter != nil {
		// non batch mode (ui.IsTerminal) may build last failed command
		// so should not trigger this check at the end of build.
		// also check it uses failureSummaryWriter (which is mainly
		// used on builder only), as we want to check this builder only.
		// developer would kill/interrupt if it won't finish.
		finished := time.Now()
		go func() {
			time.Sleep(10 * time.Minute)
			// expect siso process finishes before it runs
			fmt.Fprintf(os.Stderr, "\nBUG: http://b/360961799 - siso didn't finish in %s after build finished \ndump all goroutines:\n", time.Since(finished))
			err := pprof.Lookup("goroutine").WriteTo(os.Stderr, 1)
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to WriteTo: %v\n", err)
			}
			fmt.Fprintf(os.Stderr, "\nwait more 10 minutes.\n")
			time.Sleep(10 * time.Minute)
			fmt.Fprintf(os.Stderr, "siso still didn't finish in %s after build finished \ndump all goroutines:\n", time.Since(finished))
			err = pprof.Lookup("goroutine").WriteTo(os.Stderr, 1)
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to WriteTo: %v\n", err)
			}
			os.Exit(1)
		}()
	}
	return err
}

func (b *Builder) uploadBuildNinja(ctx context.Context) {
	started := time.Now()
	inputs := b.graph.Filenames()
	inputs = append(inputs, "args.gn")
	ents, err := b.hashFS.Entries(ctx, filepath.Join(b.path.ExecRoot, b.path.Dir), inputs)
	if err != nil {
		clog.Warningf(ctx, "failed to get build files entries: %v", err)
		return
	}
	ds := digest.NewStore()
	tree := merkletree.New(ds)
	for _, ent := range ents {
		err := tree.Set(ent)
		if err != nil {
			clog.Warningf(ctx, "failed to set %s: %v", ent.Name, err)
		}
	}
	d, err := tree.Build(ctx)
	if err != nil {
		clog.Warningf(ctx, "failed to calculate tree: %v", err)
		return
	}
	_, err = b.reapiclient.UploadAll(ctx, ds)
	if err != nil {
		clog.Warningf(ctx, "failed to upload build files tree %s: %v", d, err)
		return
	}
	if b.resultstoreUploader != nil {
		err := b.resultstoreUploader.SetFile(ctx, "build.ninja.dir", d)
		if err != nil {
			clog.Warningf(ctx, "failed to set build files tree %s: %v", d, err)
		}
	}
	clog.Infof(ctx, "uploaded build files tree %s (%d entries) in %s", d, len(ents), time.Since(started))
}

func (b *Builder) recordMetrics(ctx context.Context, m StepMetric) {
	mb, err := json.Marshal(m)
	if err != nil {
		clog.Warningf(ctx, "metrics marshal err: %v", err)
		return
	}
	fmt.Fprintf(b.metricsJSONWriter, "%s\n", mb)
}

func (b *Builder) recordCloudMonitoringActionMetrics(ctx context.Context, step *Step, actionErr error) {
	ar, cached := step.cmd.ActionResult()
	var remoteAr *rpb.ActionResult
	var remoteErr error
	if step.metrics.IsRemote {
		remoteAr = ar
		remoteErr = actionErr
	} else if step.metrics.Fallback {
		remoteAr, remoteErr = step.cmd.RemoteFallbackResult()
	}
	monitoring.ExportActionMetrics(
		ctx, time.Duration(step.metrics.Duration), ar, remoteAr, actionErr, remoteErr, cached)
}

func (b *Builder) recordNinjaLogs(ctx context.Context, s *Step) {
	// TODO: b/298594790 - Use the same mtime with hashFS.
	start := time.Duration(s.metrics.ActionStartTime).Milliseconds()
	end := time.Duration(s.metrics.ActionEndTime).Milliseconds()

	// Remove prefixed working directory path from Outputs.
	outputs := make([]string, 0, len(s.cmd.Outputs))
	buildDir := s.cmd.Dir + "/"
	for _, output := range s.cmd.Outputs {
		outputs = append(outputs, strings.TrimPrefix(output, buildDir))
	}
	ninjautil.WriteNinjaLogEntries(ctx, b.ninjaLogWriter, start, end, s.endTime, outputs, s.cmd.Args)
}

// stepLogEntry logs step in parent access log of the step.
func stepLogEntry(ctx context.Context, logger *clog.Logger, step *Step, duration time.Duration, err error) {
	httpStatus := http.StatusOK
	logEntry := logger.Entry(logging.Info, fmt.Sprintf("%s -> %v", stepDescription(step.def), err))
	if isCanceled(ctx, err) {
		logEntry.Severity = logging.Warning
		// https://cloud.google.com/apis/design/errors#handling_errors
		httpStatus = 499 // Client closed request
	} else if err != nil {
		logEntry.Severity = logging.Warning
		httpStatus = http.StatusBadRequest
	}
	logEntry.HTTPRequest = &logging.HTTPRequest{
		Request: &http.Request{
			Method: http.MethodPost,
			URL: &url.URL{
				Path: path.Join("/step", step.def.ActionName(), filepath.ToSlash(step.def.Outputs(ctx)[0])),
			},
		},
		Status: httpStatus,
		// RequestSize
		// ResponseSize
		Latency: duration,
		// CacheHit
	}
	logEntry.Labels = map[string]string{
		"id":        step.def.String(),
		"siso_rule": step.metrics.Rule,
		"action":    step.metrics.Action,
		"output":    step.metrics.Output,
		"gn_target": step.metrics.GNTarget,
		"cmdhash":   step.metrics.CmdHash,
		"digest":    step.metrics.Digest,
		"run_secs":  fmt.Sprintf("%.02f", time.Duration(step.metrics.RunTime).Seconds()),
		"exec_secs": fmt.Sprintf("%.02f", time.Duration(step.metrics.ExecTime).Seconds()),
	}
	if step.metrics.NoExec {
		logEntry.Labels["no_exec"] = "true"
	}
	if step.metrics.IsRemote {
		logEntry.Labels["is_remote"] = "true"
	}
	if step.metrics.IsLocal {
		logEntry.Labels["is_local"] = "true"
	}
	if step.metrics.FastLocal {
		logEntry.Labels["fast_local"] = "true"
	}
	if step.metrics.Cached {
		logEntry.Labels["cached"] = "true"
	}
	if step.metrics.Fallback {
		logEntry.Labels["fallback"] = "true"
	}
	if step.metrics.MaxRSS > 0 {
		logEntry.Labels["max_rss"] = strconv.FormatInt(step.metrics.MaxRSS, 10)
	}
	// TODO: record more useful metrics
	logger.Log(logEntry)
}

func isCanceled(ctx context.Context, err error) bool {
	if errors.Is(err, context.Canceled) {
		return true
	}
	st, ok := status.FromError(err)
	if ok {
		if st.Code() == codes.Canceled {
			return true
		}
	}
	select {
	case <-ctx.Done():
		return true
	default:
	}
	return false
}

// dedupInputs deduplicates inputs.
// For windows worker, which uses case insensitive file system, it also
// deduplicates filenames with different cases, e.g. "Windows.h" vs "windows.h".
// TODO(b/275452106): support Mac worker
func dedupInputs(ctx context.Context, cmd *execute.Cmd) {
	// need to dedup input with different case in intermediate dir on win and mac?
	caseInsensitive := cmd.Platform["OSFamily"] == "Windows"
	m := make(map[string]string)
	inputs := make([]string, 0, len(cmd.Inputs))
	for _, input := range cmd.Inputs {
		key := input
		if caseInsensitive {
			key = strings.ToLower(input)
		}
		if s, found := m[key]; found {
			if log.V(1) {
				clog.Infof(ctx, "dedup input %s (%s)", input, s)
			}
			continue
		}
		m[key] = input
		inputs = append(inputs, input)
	}
	cmd.Inputs = make([]string, len(inputs))
	copy(cmd.Inputs, inputs)
}

// outputs processes step's outputs.
// it will flush outputs to local disk if
// - it is specified in local outputs of StepDef.
// - it has an extension that requires scan deps of future steps.
// - it is specified by OutptutLocalFunc.
func (b *Builder) outputs(ctx context.Context, step *Step) error {
	ctx, span := trace.NewSpan(ctx, "outputs")
	defer span.Close(nil)

	outputs := step.cmd.Outputs
	if step.def.Binding("phony_outputs") != "" {
		clog.Infof(ctx, "phony_outputs. no check output files %q", outputs)
		return nil
	}

	span.SetAttr("outputs", len(outputs))
	if step.cmd.Depfile != "" {
		switch step.cmd.Deps {
		case "gcc", "msvc":
			// for deps=gcc,msvc, ninja will record it in
			// deps log and remove depfile.
		default:
			outputs = append(outputs, step.cmd.Depfile)
		}
	}

	localOutputs := step.def.LocalOutputs(ctx)
	span.SetAttr("outputs-local", len(localOutputs))
	seen := make(map[string]bool)
	for _, o := range localOutputs {
		if seen[o] {
			continue
		}
		seen[o] = true
	}

	clog.Infof(ctx, "outputs %d->%d", len(outputs), len(localOutputs))
	defOutputs := step.def.Outputs(ctx)
	// need to check against step.cmd.Outputs, not step.def.Outputs, since
	// handler may add to step.cmd.Outputs.
	for _, out := range outputs {
		// force to output local for inputs
		// .h,/.hxx/.hpp/.inc/.c/.cc/.cxx/.cpp/.m/.mm for gcc deps or msvc showIncludes
		// .json/.js/.ts for tsconfig.json, .js for grit etc.
		switch filepath.Ext(out) {
		case ".h", ".hxx", ".hpp", ".inc", ".c", ".cc", "cxx", ".cpp", ".m", ".mm", ".json", ".js", ".ts":
			if seen[out] {
				continue
			}
			localOutputs = append(localOutputs, out)
			seen[out] = true
		}
		if b.outputLocal != nil && b.outputLocal(ctx, out) {
			if seen[out] {
				continue
			}
			localOutputs = append(localOutputs, out)
			seen[out] = true
		}
		_, err := b.hashFS.Stat(ctx, step.cmd.ExecRoot, out)
		if err != nil {
			reqOut := false
			for _, o := range defOutputs {
				if out == o {
					reqOut = true
					break
				}
			}
			if !reqOut {
				clog.Warningf(ctx, "missing outputs %s: %v", out, err)
				outs := make([]string, 0, len(localOutputs))
				for _, f := range localOutputs {
					if f == out {
						continue
					}
					outs = append(outs, f)
				}
				localOutputs = outs
				continue
			}
			return fmt.Errorf("missing outputs %s: %w", out, err)
		}
	}
	if len(localOutputs) > 0 {
		err := b.hashFS.Flush(ctx, step.cmd.ExecRoot, localOutputs)
		if err != nil {
			return fmt.Errorf("failed to flush outputs to local: %w", err)
		}
	}
	return nil
}

// progressStepCacheHit shows progress of the cache hit step.
func (b *Builder) progressStepCacheHit(ctx context.Context, step *Step) {
	b.progress.step(ctx, b, step, progressPrefixCacheHit+step.cmd.Desc)
}

// progressStepStarted shows progress of the started step.
func (b *Builder) progressStepStarted(ctx context.Context, step *Step) {
	step.setPhase(stepStart)
	b.progress.step(ctx, b, step, progressPrefixStart+step.cmd.Desc)
}

// progressStepFinished shows progress of the finished step.
func (b *Builder) progressStepFinished(ctx context.Context, step *Step) {
	step.setPhase(stepDone)
	b.progress.step(ctx, b, step, progressPrefixFinish+step.cmd.Desc)
}

// progressStepRetry shows progress of the retried step.
func (b *Builder) progressStepRetry(ctx context.Context, step *Step) {
	b.progress.step(ctx, b, step, progressPrefixRetry+step.cmd.Desc)
}

// progressStepFallback shows progress of the fallback step.
func (b *Builder) progressStepFallback(ctx context.Context, step *Step) {
	b.progress.step(ctx, b, step, progressPrefixFallback+step.cmd.Desc)
}

var errNotRelocatable = errors.New("request is not relocatable")

func (b *Builder) updateDeps(ctx context.Context, step *Step) error {
	ctx, span := trace.NewSpan(ctx, "update-deps")
	defer span.Close(nil)
	if len(step.cmd.Outputs) == 0 {
		clog.Warningf(ctx, "update deps: no outputs")
		return nil
	}
	output, err := filepath.Rel(step.cmd.Dir, step.cmd.Outputs[0])
	if err != nil {
		clog.Warningf(ctx, "update deps: failed to get rel %s,%s: %v", step.cmd.Dir, step.cmd.Outputs[0], err)
		return nil
	}
	fi, err := b.hashFS.Stat(ctx, step.cmd.ExecRoot, step.cmd.Outputs[0])
	if err != nil {
		clog.Warningf(ctx, "update deps: missing outputs %s: %v", step.cmd.Outputs[0], err)
		return nil
	}
	deps, err := depsAfterRun(ctx, b, step)
	if err != nil {
		return err
	}
	updated, err := step.def.RecordDeps(ctx, output, fi.ModTime(), deps)
	if err != nil {
		clog.Warningf(ctx, "update deps: failed to record deps %s, %s, %s, %s: %v", output, base64.StdEncoding.EncodeToString(step.cmd.CmdHash), fi.ModTime(), deps, err)
	}
	clog.Infof(ctx, "update deps=%s: %s %s %d updated:%t pure:%t/%t->true", step.cmd.Deps, output, base64.StdEncoding.EncodeToString(step.cmd.CmdHash), len(deps), updated, step.cmd.Pure, step.cmd.Pure)
	span.SetAttr("deps", len(deps))
	span.SetAttr("updated", updated)
	canonicalizedDeps := make([]string, 0, len(deps))
	for _, dep := range deps {
		canonicalizedDeps = append(canonicalizedDeps, b.path.MaybeFromWD(ctx, dep))
	}
	depsFixCmd(ctx, b, step, canonicalizedDeps)
	return nil
}

func (b *Builder) finalizeTrace(ctx context.Context, tc *trace.Context) {
	b.traceEvents.Add(ctx, tc)
	b.traceStats.update(ctx, tc)
	b.traceExporter.Export(ctx, tc)
	b.tracePprof.Add(ctx, tc)
}

func (b *Builder) prepareAllOutDirs(ctx context.Context) error {
	started := time.Now()
	seen := make(map[string]struct{})
	// Collect only the deepest directories to avoid redundant `os.MkdirAll`.
	for target, ti := range b.plan.targets {
		if !ti.output {
			continue
		}
		p, err := b.graph.TargetPath(ctx, Target(target))
		if err != nil {
			return err
		}
		seen[filepath.Dir(p)] = struct{}{}
	}
	dirs := make([]string, 0, len(seen))
	for dir := range seen {
		dirs = append(dirs, dir)
	}
	ndirs := len(dirs)
	sort.Strings(dirs)
	// Delete intermediate directories of `dir` from `seen`, because
	// `os.MkdirAll` will create the intermediate directories anyway.
	for _, dir := range dirs {
		for {
			pdir := filepath.Dir(dir)
			_, found := seen[pdir]
			if !found {
				break
			}
			delete(seen, pdir)
			dir = pdir
		}
	}
	dirs = dirs[:0]
	for dir := range seen {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	clog.Infof(ctx, "prepare out dirs: targets:%d -> %d -> %d ", len(b.plan.targets), ndirs, len(dirs))
	for _, dir := range dirs {
		// we don't use hashfs here for performance.
		// just create dirs on local disk, so reproxy and local process
		// can detect the dirs.
		err := os.MkdirAll(filepath.Join(b.path.ExecRoot, dir), 0755)
		if err != nil {
			return err
		}
	}
	clog.Infof(ctx, "prepare out dirs %d in %s", len(dirs), time.Since(started))
	return nil
}

func (b *Builder) ActiveSteps() []ActiveStepInfo {
	return b.progress.ActiveSteps()
}

func (b *Builder) localFallbackEnabled() bool {
	return !experiments.Enabled("no-fallback", "") && !b.hashFS.OnCog()
}
