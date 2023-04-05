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
	"os"
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

	"infra/build/siso/execute"
	"infra/build/siso/execute/localexec"
	"infra/build/siso/execute/remoteexec"
	"infra/build/siso/hashfs"
	"infra/build/siso/o11y/clog"
	"infra/build/siso/o11y/iometrics"
	"infra/build/siso/o11y/pprof"
	"infra/build/siso/o11y/trace"
	"infra/build/siso/reapi"
	"infra/build/siso/sync/semaphore"
	"infra/build/siso/toolsupport/gccutil"
	"infra/build/siso/toolsupport/msvcutil"
	"infra/build/siso/ui"
)

// logging labels's key.
const (
	logLabelKeyID        = "id"
	logLabelKeyBacktrace = "backtrace"
)

const (
	// limit # of concurrent steps at most 1024 times of num cpus
	// to protect from out of memory, or too many threads.
	stepLimitFactor = 1024

	// limit # of concurrent steps at most 40 times of num cpus
	// to protect from out of memory, or DDoS to RE API.
	remoteLimitFactor = 40
)

// OutputLocalFunc is a function to determine the file should be downloaded or not.
type OutputLocalFunc func(context.Context, string) bool

// Options is builder options.
type Options struct {
	ID                string
	Metadata          Metadata
	ProjectID         string
	Path              *Path
	HashFS            *hashfs.HashFS
	REAPIClient       *reapi.Client
	RECacheEnableRead bool
	// TODO(b/266518906): enable RECacheEnableWrite option for read-only client.
	// RECacheEnableWrite bool
	ActionSalt []byte
	// TODO(b/266518906): enable shared deps log
	// SharedDepsLog      SharedDepsLog
	OutputLocal        OutputLocalFunc
	Cache              *Cache
	LocalexecLogWriter io.Writer
	MetricsJSONWriter  io.Writer
	TraceJSON          string
	Pprof              string
	// TODO(b/266518906): add exporter
	// TraceExporter      *trace.Exporter
	PprofUploader *pprof.Uploader

	// Clobber forces to rebuild ignoring existing generated files.
	Clobber bool

	// DryRun just prints the command to build, but does nothing.
	DryRun bool

	// RebuildManifest is a build manifest filename (i.e. build.ninja)
	// when rebuilding manifest.
	// empty for normal build.
	RebuildManifest string
}

var experiments Experiments

// Metadata is a metadata of the build process.
type Metadata struct {
	// KV is an arbitrary key-value pair in the metadata.
	KV     map[string]string
	NumCPU int
	GOOS   string
	GOARCH string
	// want to include hostname?
}

// Get returns a map of the metadata.
func (md Metadata) Get() map[string]string {
	m := make(map[string]string)
	for k, v := range md.KV {
		m[k] = v
	}
	m["numcpu"] = fmt.Sprintf("%d", md.NumCPU)
	m["goos"] = md.GOOS
	m["goarch"] = md.GOARCH
	return m
}

// Builder is a builder.
type Builder struct {
	// build session id, tool invocation id.
	id        string
	projectID string
	metadata  Metadata

	w        io.Writer
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

	localSema *semaphore.Semaphore
	localExec localexec.LocalExec

	remoteSema        *semaphore.Semaphore
	remoteExec        *remoteexec.RemoteExec
	reCacheEnableRead bool
	// TODO(b/266518906): enable reCacheEnableWrite option for read-only client.
	// reCacheEnableWrite bool
	reapiclient *reapi.Client

	actionSalt []byte

	sharedDepsLog SharedDepsLog

	outputLocal OutputLocalFunc

	cacheSema *semaphore.Semaphore
	cache     *Cache

	localexecLogWriter io.Writer
	metricsJSONWriter  io.Writer
	traceEvents        *traceEvents
	traceStats         *traceStats
	tracePprof         *tracePprof
	pprofUploader      *pprof.Uploader

	clobber         bool
	dryRun          bool
	rebuildManifest string
}

// New creates new builder.
func New(ctx context.Context, graph Graph, opts Options) (*Builder, error) {
	logger := clog.FromContext(ctx)
	if logger != nil {
		logger.Formatter = logFormat
	}
	lelw := opts.LocalexecLogWriter
	if lelw == nil {
		lelw = io.Discard
	}
	mw := opts.MetricsJSONWriter
	if mw == nil {
		mw = io.Discard
	}

	if err := opts.Path.Check(); err != nil {
		return nil, err
	}
	if opts.HashFS == nil {
		return nil, fmt.Errorf("hash fs must be set")
	}
	var le localexec.LocalExec
	var re *remoteexec.RemoteExec
	if opts.REAPIClient != nil {
		logger.Infof("enable remote exec")
		re = remoteexec.New(ctx, opts.REAPIClient)
	} else {
		logger.Infof("disable remote exec")
	}
	experiments.ShowOnce()
	numCPU := runtime.NumCPU()
	stepLimit := stepLimitFactor * numCPU
	localLimit := numCPU
	remoteLimit := remoteLimitFactor * numCPU
	// on many cores machine, it would hit default max thread limit = 10000
	// usually, it would require 1/3 threads of stepLimit (cache miss case?)
	maxThreads := stepLimit / 3
	if maxThreads > 10000 {
		debug.SetMaxThreads(maxThreads)
	} else {
		maxThreads = 10000
	}
	logger.Infof("numcpu=%d threads:%d - step limit=%d local limit=%d remote limit=%d",
		numCPU, maxThreads, stepLimit, localLimit, remoteLimit)
	logger.Infof("tool_invocation_id: %s", opts.ID)

	return &Builder{
		id:        opts.ID,
		projectID: opts.ProjectID,
		metadata:  opts.Metadata,

		path:              opts.Path,
		hashFS:            opts.HashFS,
		graph:             graph,
		stepSema:          semaphore.New("step", stepLimit),
		preprocSema:       semaphore.New("preproc", stepLimit),
		localSema:         semaphore.New("localexec", localLimit),
		localExec:         le,
		remoteSema:        semaphore.New("remoteexec", remoteLimit),
		remoteExec:        re,
		reCacheEnableRead: opts.RECacheEnableRead,
		// reCacheEnableWrite: opts.RECacheEnableWrite,
		actionSalt:  opts.ActionSalt,
		reapiclient: opts.REAPIClient,
		// sharedDepsLog:      opts.SharedDepsLog,
		outputLocal:        opts.OutputLocal,
		cacheSema:          semaphore.New("cache", stepLimit),
		cache:              opts.Cache,
		localexecLogWriter: lelw,
		metricsJSONWriter:  mw,
		// traceExporter:      opts.TraceExporter,
		traceEvents:     newTraceEvents(opts.TraceJSON, opts.Metadata),
		traceStats:      newTraceStats(),
		tracePprof:      newTracePprof(opts.Pprof),
		pprofUploader:   opts.PprofUploader,
		clobber:         opts.Clobber,
		dryRun:          opts.DryRun,
		rebuildManifest: opts.RebuildManifest,
	}, nil
}

// Closes closes the builder.
func (b *Builder) Close(ctx context.Context) error {
	if b.dryRun {
		return nil
	}
	return b.hashFS.Close(ctx)
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
	defer func() {
		if r := recover(); r != nil {
			clog.Errorf(ctx, "panic in build: %v", r)
			if err == nil {
				err = fmt.Errorf("panic in build: %v", r)
			}
		}
		clog.Infof(ctx, "build %v", err)
	}()
	started := time.Now()
	// scheduling
	// TODO: run asynchronously?
	schedOpts := schedulerOption{
		Path:   b.path,
		HashFS: b.hashFS,
		// TODO(b/267576561): enable trace export
	}
	sched := newScheduler(ctx, schedOpts)
	err = schedule(ctx, sched, b.graph, args...)
	if err != nil {
		return err
	}
	b.plan = sched.plan
	b.stats = sched.stats

	stat := b.Stats()
	if stat.Total == 0 {
		return fmt.Errorf("nothing to build for %q", args)
	}
	ui.PrintLines("\n", fmt.Sprintf("%s %d\n", name, stat.Total), "")
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
				clog.Warningf(ctx, "failed to stat %s: %v", b.rebuildManifest, err)
				return
			}
			clog.Infof(ctx, "rebuild manifest %#v %s: %s->%s: %s", stat, b.rebuildManifest, mftime, fi.ModTime(), time.Since(started))
			if fi.ModTime().After(mftime) || stat.Done != stat.Skipped {
				ui.PrintLines(fmt.Sprintf("updated %s\n", time.Since(started)))
				err = ErrManifestModified
				return
			}
			return
		}
		clog.Infof(ctx, "build %s: %v", time.Since(started), err)
		restat := b.reapiclient.IOMetrics().Stats()
		ui.PrintLines(
			fmt.Sprintf("run:%d+%d pure:%d fastDeps:%d+%d cache:%d fallback:%d skip:%d\n",
				stat.Local, stat.Remote, stat.Pure, stat.FastDepsSuccess, stat.FastDepsFailed, stat.CacheHit, stat.LocalFallback, stat.Skipped) +
				fmt.Sprintf("reapi: ops: %d(err:%d) / r:%d(err:%d) %s / w:%d(err:%d) %s\n",
					restat.Ops, restat.OpsErrs,
					restat.ROps, restat.RErrs, numBytes(restat.RBytes),
					restat.WOps, restat.WErrs, numBytes(restat.WBytes)) +
				fmt.Sprintf("total:%d in %s: %v\n",
					stat.Total, time.Since(started), err))
	}()
	semas := []*semaphore.Semaphore{
		b.stepSema,
		b.localSema,
		b.remoteSema,
		b.cacheSema,
		b.cache.Sema,
		gccutil.Semaphore,
		hashfs.FlushSemaphore,
		msvcutil.Semaphore,
		remoteexec.Semaphore,
	}
	b.traceEvents.Start(ctx, semas, []*iometrics.IOMetrics{
		b.hashFS.IOMetrics,
		b.reapiclient.IOMetrics(),
		// XXX: cache iometrics?
	})
	defer b.traceEvents.Close(ctx)
	b.tracePprof.SetMetadata(b.metadata)
	b.pprofUploader.SetMetadata(ctx, b.metadata.Get())
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
	b.start = time.Now()
	pstat := b.plan.stats()
	b.progress.report("[%d+%d] build start", pstat.npendings, pstat.nready)
	clog.Infof(ctx, "build pendings=%d ready=%d", pstat.npendings, pstat.nready)
	b.progress.start(ctx, b)
	defer b.progress.stop(ctx)
	var wg sync.WaitGroup
	errch := make(chan error, 1000)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
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
				done()
				break loop
			}
		case err := <-errch:
			clog.Infof(ctx, "err from errch: %v", err)
			done()
			cancel()
			return err
		case <-ctx.Done():
			clog.Infof(ctx, "context done")
			done()
			cancel()
			b.plan.dump(ctx)
			return ctx.Err()
		}
		b.plan.pushReady()
		wg.Add(1)
		go func(step *Step) {
			defer wg.Done()
			defer done()
			stepStart := time.Now()
			tc := trace.New(ctx, step.Def.String())
			ctx := trace.NewContext(ctx, tc)
			spanName := stepSpanName(step.Def)
			ctx, span := trace.NewSpan(ctx, "step:"+spanName)
			traceID, spanID := span.ID(b.projectID)
			sctx := clog.NewSpan(ctx, traceID, spanID, map[string]string{
				"id": step.Def.String(),
			})
			logger := clog.FromContext(sctx)
			if logger.Formatter == nil {
				logger.Formatter = logFormat
			}
			logEntry := logger.Entry(logging.Info, step.Def.Binding("description"))
			logEntry.Labels = map[string]string{
				"id":          step.Def.String(),
				"command":     step.Def.Binding("command"),
				"description": step.Def.Binding("description"),
				"action":      step.Def.ActionName(),
				"span_name":   spanName,
				"output0":     step.Def.Outputs()[0],
			}
			logger.Log(logEntry)
			step.Metrics.BuildID = b.id
			step.Metrics.StepID = step.Def.String()
			step.Metrics.Action = step.Def.ActionName()
			step.Metrics.Output = step.Def.Outputs()[0]
			step.Metrics.PrevStepID = step.PrevStepID
			step.Metrics.PrevStepOut = step.PrevStepOut
			step.Metrics.Ready = IntervalMetric(step.ReadyTime.Sub(started))
			step.Metrics.Start = IntervalMetric(stepStart.Sub(step.ReadyTime))

			span.SetAttr("ready_time", time.Since(step.ReadyTime).Milliseconds())
			span.SetAttr("prev", step.PrevStepID)
			span.SetAttr("prev_out", step.PrevStepOut)
			span.SetAttr("queue_time", time.Since(step.QueueTime).Milliseconds())
			span.SetAttr("queue_size", step.QueueSize)
			span.SetAttr("build_id", b.id)
			span.SetAttr("id", step.Def.String())
			span.SetAttr("command", step.Def.Binding("command"))
			span.SetAttr("description", step.Def.Binding("description"))
			span.SetAttr("action", step.Def.ActionName())
			span.SetAttr("span_name", spanName)
			span.SetAttr("output0", step.Def.Outputs()[0])
			if next := step.Def.Next(); next != nil {
				span.SetAttr("next_id", step.Def.Next().String())
			}
			span.SetAttr("backtraces", stepBacktraces(step))
			err := b.runStep(sctx, step)
			span.Close(nil)
			duration := time.Since(stepStart)
			stepLogEntry(sctx, logger, step, duration, err)

			if !step.Def.IsPhony() && !step.Metrics.Skip {
				// $ cat siso_metrcis.json |
				//     jq --slurp 'sort_by(.duration)|reverse'
				//
				//     jq --slurp 'sort_by(.duration) | reverse | .[] | select(.cached==false)'
				step.Metrics.Duration = IntervalMetric(duration)
				step.Metrics.Err = err != nil
				mb, err := json.Marshal(step.Metrics)
				if err != nil {
					clog.Warningf(ctx, "metrics marshal err: %v", err)
				} else {
					fmt.Fprintf(b.metricsJSONWriter, "%s\n", mb)
				}
			}

			select {
			case <-ctx.Done():
				return
			default:
			}
			b.traceEvents.Add(ctx, tc)
			b.traceStats.update(ctx, tc)
			// TODO(b/267576561): export trace
			b.tracePprof.Add(ctx, tc)
			tc = nil
			step.Cmd = nil
			if err != nil {
				select {
				case <-ctx.Done():
				case errch <- err:
				default:
					clog.Warningf(ctx, "failed to send err channel: %v", err)
				}
			}
		}(step)
	}
	clog.Infof(ctx, "all pendings becomes ready")
	wg.Wait()
	close(errch)
	err = <-errch
	b.progress.step(ctx, b, nil, fmt.Sprintf("build finished: %v\n", err))
	return err
}

// stepLogEntry logs step in parent access log of the step.
func stepLogEntry(ctx context.Context, logger *clog.Logger, step *Step, duration time.Duration, err error) {
	// TODO(b/267575656): implement this for cloud logging.
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
	span.SetAttr("outputs", len(step.Cmd.Outputs))
	localOutputs := step.Def.LocalOutputs()
	span.SetAttr("outputs-local", len(localOutputs))
	seen := make(map[string]bool)
	for _, o := range localOutputs {
		if seen[o] {
			continue
		}
		seen[o] = true
	}

	clog.Infof(ctx, "outputs %d->%d", len(step.Cmd.Outputs), len(localOutputs))
	allowMissing := step.Def.Binding("allow_missing_outputs") != ""
	// need to check against step.cmd.Outputs, not step.def.Outputs, since
	// handler may add to step.cmd.Outputs.
	for _, out := range step.Cmd.Outputs {
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
		_, err := b.hashFS.Stat(ctx, step.Cmd.ExecRoot, out)
		if err != nil {
			if allowMissing {
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
		err := b.hashFS.Flush(ctx, step.Cmd.ExecRoot, localOutputs)
		if err != nil {
			return fmt.Errorf("failed to flush outputs to local: %w", err)
		}
	}
	return nil
}

// progressStepCacheHit shows progress of the cache hit step.
func (b *Builder) progressStepCacheHit(ctx context.Context, step *Step) {
	b.progress.step(ctx, b, step, "c "+step.Cmd.Desc)
}

// progressStepCacheHit shows progress of the skipped step.
func (b *Builder) progressStepSkipped(ctx context.Context, step *Step) {
	b.progress.step(ctx, b, step, "- "+step.Cmd.Desc)
}

// progressStepCacheHit shows progress of the started step.
func (b *Builder) progressStepStarted(ctx context.Context, step *Step) {
	step.SetPhase(stepStart)
	step.StartTime = time.Now()
	b.progress.step(ctx, b, step, "S "+step.Cmd.Desc)
}

// progressStepCacheHit shows progress of the finished step.
func (b *Builder) progressStepFinished(ctx context.Context, step *Step) {
	step.SetPhase(stepDone)
	b.progress.step(ctx, b, step, "F "+step.Cmd.Desc)
}

var errNotRelocatable = errors.New("request is not relocatable")

func (b *Builder) updateDeps(ctx context.Context, step *Step) error {
	ctx, span := trace.NewSpan(ctx, "update-deps")
	defer span.Close(nil)
	if len(step.Cmd.Outputs) == 0 {
		clog.Warningf(ctx, "update deps: no outputs")
		return nil
	}
	output, err := filepath.Rel(step.Cmd.Dir, step.Cmd.Outputs[0])
	if err != nil {
		clog.Warningf(ctx, "update deps: failed to get rel %s,%s: %v", step.Cmd.Dir, step.Cmd.Outputs[0], err)
		return nil
	}
	fi, err := b.hashFS.Stat(ctx, step.Cmd.ExecRoot, step.Cmd.Outputs[0])
	if err != nil {
		clog.Warningf(ctx, "update deps: missing outputs %s: %v", step.Cmd.Outputs[0], err)
		return nil
	}
	deps, err := depsAfterRun(ctx, b, step)
	if err != nil {
		clog.Warningf(ctx, "update deps: %v", err)
		return err
	}
	if len(deps) == 0 {
		return nil
	}
	var updated bool
	if step.FastDeps {
		// if fastDeps case, we already know the correct deps for this cmd.
		// just update for local deps log for incremental build.
		updated, err = step.Def.RecordDeps(ctx, output, fi.ModTime(), deps)
	} else {
		// otherwise, update both local and shared.
		updated, err = b.recordDepsLog(ctx, step.Def, output, step.Cmd.CmdHash, fi.ModTime(), deps)
	}
	if err != nil {
		clog.Warningf(ctx, "update deps: failed to record deps %s, %s, %s, %s: %v", output, hex.EncodeToString(step.Cmd.CmdHash), fi.ModTime(), deps, err)
	}
	clog.Infof(ctx, "update deps=%s: %s %s %d updated:%t pure:%t/%t->true", step.Cmd.Deps, output, hex.EncodeToString(step.Cmd.CmdHash), len(deps), updated, step.Cmd.Pure, step.Cmd.Pure)
	span.SetAttr("deps", len(deps))
	span.SetAttr("updated", updated)
	for i := range deps {
		deps[i] = b.path.MustFromWD(deps[i])
	}
	depsFixCmd(ctx, b, step, deps)
	return nil
}

func (b *Builder) phonyDone(ctx context.Context, step *Step) error {
	if log.V(1) {
		clog.Infof(ctx, "step phony %s", step)
	}
	b.plan.done(ctx, step, step.Def.Outputs())
	return nil
}

func (b *Builder) done(ctx context.Context, step *Step) error {
	ctx, span := trace.NewSpan(ctx, "done")
	defer span.Close(nil)
	var outputs []string
	allowMissing := step.Def.Binding("allow_missing_outputs") != ""
	for _, out := range step.Cmd.Outputs {
		out := out
		var mtime time.Time
		if log.V(1) {
			clog.Infof(ctx, "output -> %s", out)
		}
		fi, err := b.hashFS.Stat(ctx, step.Cmd.ExecRoot, out)
		if err != nil {
			if allowMissing {
				clog.Warningf(ctx, "missing output %s: %v", out, err)
				continue
			}
			if !b.dryRun {
				return fmt.Errorf("output %s for %s: %w", out, step, err)
			}
		}
		if fi != nil {
			mtime = fi.ModTime()
		}
		if log.V(1) {
			clog.Infof(ctx, "become ready: %s %s", out, mtime)
		}
		outputs = append(outputs, out)
	}
	b.stats.done(step.Cmd.Pure)
	b.plan.done(ctx, step, outputs)
	return nil
}

func (b *Builder) failedToRun(ctx context.Context, cmd *execute.Cmd, err error) {
	clog.Warningf(ctx, "Failed to exec: %v", err)
	if errors.Is(err, context.Canceled) || ctx.Err() != nil {
		return
	}
	var output string
	if len(cmd.Outputs) > 0 {
		output = cmd.Outputs[0]
		if strings.HasPrefix(output, cmd.Dir+"/") {
			output = "./" + strings.TrimPrefix(output, cmd.Dir+"/")
		}
	}
	var msgs []string
	msgs = append(msgs, "\n", fmt.Sprintf("\nFAILED: %s %s\n%q %q\n%q\n", cmd, cmd.Desc, cmd.ActionName, output, cmd.Command()))
	rsp := cmd.RSPFile
	if rsp != "" {
		msgs = append(msgs, fmt.Sprintf(" %s=%q\n", rsp, cmd.RSPFileContent))
	}
	ui.PrintLines(msgs...)
	os.Stdout.Write(cmd.Stdout())
	os.Stderr.Write(append(cmd.Stderr(), '\n'))
}
