// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/logging"
	log "github.com/golang/glog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"infra/build/siso/build/metadata"
	"infra/build/siso/execute"
	"infra/build/siso/execute/localexec"
	"infra/build/siso/execute/remoteexec"
	"infra/build/siso/execute/reproxyexec"
	"infra/build/siso/hashfs"
	"infra/build/siso/o11y/clog"
	"infra/build/siso/o11y/iometrics"
	"infra/build/siso/o11y/pprof"
	"infra/build/siso/o11y/trace"
	"infra/build/siso/reapi"
	"infra/build/siso/reapi/digest"
	"infra/build/siso/reapi/merkletree"
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
	OutputLogWriter      io.Writer
	ExplainWriter        io.Writer
	LocalexecLogWriter   io.Writer
	MetricsJSONWriter    io.Writer
	NinjaLogWriter       io.Writer
	TraceJSON            string
	Pprof                string
	TraceExporter        *trace.Exporter
	PprofUploader        *pprof.Uploader

	// Clobber forces to rebuild ignoring existing generated files.
	Clobber bool

	// Verbose shows all command lines while building rather than step description.
	Verbose bool

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

var experiments Experiments

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

	start time.Time
	graph Graph
	plan  *plan
	stats *stats

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
	localexecLogWriter   io.Writer
	metricsJSONWriter    io.Writer
	ninjaLogWriter       io.Writer
	outputLogWriter      io.Writer
	traceExporter        *trace.Exporter
	traceEvents          *traceEvents
	traceStats           *traceStats
	tracePprof           *tracePprof
	pprofUploader        *pprof.Uploader

	clobber bool
	verbose bool
	dryRun  bool

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
		logger.Infof("enable remote exec")
		re = remoteexec.New(ctx, opts.REAPIClient)
	} else {
		logger.Infof("disable remote exec")
	}
	pe = reproxyexec.New(ctx, opts.ReproxyAddr)
	experiments.ShowOnce()
	numCPU := runtime.NumCPU()
	if (opts.Limits == Limits{}) {
		opts.Limits = DefaultLimits(ctx)
	}
	// on many cores machine, it would hit default max thread limit = 10000
	// usually, it would require 1/3 threads of stepLimit (cache miss case?)
	maxThreads := opts.Limits.Step / 3
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
		clobber:              opts.Clobber,
		verbose:              opts.Verbose,
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
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	defer func() {
		if r := recover(); r != nil {
			const size = 64 << 10
			buf := make([]byte, size)
			buf = buf[:runtime.Stack(buf, false)]
			clog.Errorf(ctx, "panic in build: %v\n%s", r, buf)
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
		Path:   b.path,
		HashFS: b.hashFS,
	}
	sched := newScheduler(ctx, schedOpts)
	err = schedule(ctx, sched, b.graph, args...)
	if err != nil {
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
			clog.Infof(ctx, "rebuild manifest %#v %s: %s->%s: %s", stat, b.rebuildManifest, mftime, fi.ModTime(), time.Since(b.start))
			if fi.ModTime().After(mftime) || stat.Done != stat.Skipped {
				ui.Default.PrintLines(fmt.Sprintf("%6s Regenerating ninja files\n", ui.FormatDuration(time.Since(b.start))))
				err = ErrManifestModified
				return
			}
			return
		}
		clog.Infof(ctx, "build %s: %v", time.Since(b.start), err)
		if stat.Skipped == stat.Total {
			ui.Default.PrintLines(ninjaNoWorkToDo)
			return
		}
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
			ui.Default.PrintLines(
				"\n",
				fmt.Sprintf("\nlocal:%d remote:%d cache:%d fallback:%d skip:%d\n",
					stat.Local+stat.NoExec, stat.Remote, stat.CacheHit, stat.LocalFallback, stat.Skipped)+
					depsStatLine+
					restatLine)
		} else {
			ui.Default.PrintLines("\n", "\n")
		}
	}()
	semas := []*semaphore.Semaphore{
		b.stepSema,
		b.localSema,
		b.rewrapSema,
		b.remoteSema,
		b.cacheSema,
		b.cache.sema,
		hashfs.FlushSemaphore,
		remoteexec.Semaphore,
	}
	b.traceEvents.Start(ctx, semas, []*iometrics.IOMetrics{
		b.hashFS.IOMetrics,
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
	b.progress.report("[%d+%d] build start", pstat.npendings, pstat.nready)
	clog.Infof(ctx, "build pendings=%d ready=%d", pstat.npendings, pstat.nready)
	b.progress.start(ctx, b)
	defer b.progress.stop(ctx)

	if b.clobber {
		fmt.Fprintf(b.explainWriter, "--clobber is specified\n")
	}
	var wg sync.WaitGroup
	var errs []error
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
					errs = append(errs, err)
				}
			}
			numServs := b.stepSema.NumServs()
			hasReady := b.plan.hasReady()
			// no active steps and no ready steps?
			stuck := numServs == 0 && !hasReady
			if stuck && len(errs) > 0 {
				errs = append([]error{errors.New("cannot make progress due to previous errors")}, errs...)
			}
			if log.V(1) {
				clog.Infof(ctx, "errs=%d numServs=%d hasReady=%t stuck=%t", len(errs), numServs, hasReady, stuck)
			}
			if len(errs) >= b.failuresAllowed || stuck {
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
					clog.Errorf(ctx, "runStep panic: %v\nstep.cmd: %p\n%s", r, step.cmd, buf)
					err = fmt.Errorf("panic: %v: %s", r, buf)
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
			if len(errs) >= b.failuresAllowed {
				continue
			}
			if e != nil && !errors.Is(e, context.Canceled) {
				errs = append(errs, e)
			}
		}
		errdone <- errors.Join(errs...)
	}()
	wg.Wait()
	close(errch)
	err = <-errdone
	if !b.verbose {
		// replace 2 progress lines.
		ui.Default.PrintLines(fmt.Sprintf("%s finished: %v", name, err), "")
	}
	// metrics for full build session, without step_id etc.
	var metrics StepMetric
	metrics.BuildID = b.id
	metrics.Duration = IntervalMetric(time.Since(b.start))
	metrics.Err = err != nil
	b.recordMetrics(ctx, metrics)
	clog.Infof(ctx, "%s finished: %v", name, err)
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

func (b *Builder) recordNinjaLogs(ctx context.Context, s *Step) {
	start := time.Duration(s.metrics.ActionStartTime).Milliseconds()
	end := time.Duration(s.metrics.ActionEndTime).Milliseconds()
	// TODO: b/298594790 - Use the same mtime with hashFS.
	// end time = start time + duration
	mtime := s.startTime.Add(time.Duration(s.metrics.Duration))

	ninjautil.WriteNinjaLogEntries(ctx, b.ninjaLogWriter, start, end, mtime, s.cmd.Outputs, s.cmd.Args)
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
				Path: path.Join("/step", step.def.ActionName(), filepath.ToSlash(step.def.Outputs()[0])),
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

	localOutputs := step.def.LocalOutputs()
	span.SetAttr("outputs-local", len(localOutputs))
	seen := make(map[string]bool)
	for _, o := range localOutputs {
		if seen[o] {
			continue
		}
		seen[o] = true
	}

	clog.Infof(ctx, "outputs %d->%d", len(outputs), len(localOutputs))
	defOutputs := step.def.Outputs()
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
	step.startTime = time.Now()
	b.progress.step(ctx, b, step, progressPrefixStart+step.cmd.Desc)
}

// progressStepFinished shows progress of the finished step.
func (b *Builder) progressStepFinished(ctx context.Context, step *Step) {
	step.setPhase(stepDone)
	b.progress.step(ctx, b, step, progressPrefixFinish+step.cmd.Desc)
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
		clog.Warningf(ctx, "update deps: %v", err)
		return err
	}
	updated, err := step.def.RecordDeps(ctx, output, fi.ModTime(), deps)
	if err != nil {
		clog.Warningf(ctx, "update deps: failed to record deps %s, %s, %s, %s: %v", output, hex.EncodeToString(step.cmd.CmdHash), fi.ModTime(), deps, err)
	}
	clog.Infof(ctx, "update deps=%s: %s %s %d updated:%t pure:%t/%t->true", step.cmd.Deps, output, hex.EncodeToString(step.cmd.CmdHash), len(deps), updated, step.cmd.Pure, step.cmd.Pure)
	span.SetAttr("deps", len(deps))
	span.SetAttr("updated", updated)
	for i := range deps {
		deps[i] = b.path.MustFromWD(deps[i])
	}
	depsFixCmd(ctx, b, step, deps)
	return nil
}

func (b *Builder) finalizeTrace(ctx context.Context, tc *trace.Context) {
	b.traceEvents.Add(ctx, tc)
	b.traceStats.update(ctx, tc)
	b.traceExporter.Export(ctx, tc)
	b.tracePprof.Add(ctx, tc)
}
