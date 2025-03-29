// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/log"

	"go.chromium.org/infra/build/siso/build/metadata"
	"go.chromium.org/infra/build/siso/execute"
	"go.chromium.org/infra/build/siso/execute/localexec"
	"go.chromium.org/infra/build/siso/execute/remoteexec"
	"go.chromium.org/infra/build/siso/hashfs"
	"go.chromium.org/infra/build/siso/reapi"
	"go.chromium.org/infra/build/siso/reapi/digest"
	"go.chromium.org/infra/build/siso/reapi/merkletree"
	"go.chromium.org/infra/build/siso/scandeps"
	"go.chromium.org/infra/build/siso/sync/semaphore"
	"go.chromium.org/infra/build/siso/ui"
)

// chromium recipe module expects this string.
const ninjaNoWorkToDo = "ninja: no work to do."

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
	ActionSalt []byte

	OutputLocal OutputLocalFunc
	Cache       *Cache

	// Clobber forces to rebuild ignoring existing generated files.
	Clobber bool

	// Build inputs of targets, but not build targets itself.
	Prepare bool

	// Verbose shows all command lines while building rather than step description.
	Verbose bool

	// DryRun just prints the command to build, but does nothing.
	DryRun bool

	// allow failures at most FailuresAllowed.
	FailuresAllowed int

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

	// for subtree: dir -> *subtree
	trees sync.Map

	scanDeps *scandeps.ScanDeps

	localSema *semaphore.Semaphore
	poolSemas map[string]*semaphore.Semaphore
	localExec localexec.LocalExec

	remoteSema        *semaphore.Semaphore
	remoteExec        *remoteexec.RemoteExec
	reCacheEnableRead bool
	// TODO(b/266518906): enable reCacheEnableWrite option for read-only client.
	// reCacheEnableWrite bool
	reapiclient *reapi.Client

	actionSalt []byte

	outputLocal OutputLocalFunc

	cache *Cache

	// envfiles: filename -> *envfile
	envFiles sync.Map

	clobber bool
	prepare bool
	verbose bool
	dryRun  bool

	failures failures

	rebuildManifest string
}

// New creates new builder.
func New(ctx context.Context, graph Graph, opts Options) (*Builder, error) {
	start := opts.StartTime
	if start.IsZero() {
		start = time.Now()
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
		log.Infof("remote execution enabled")
		re = remoteexec.New(opts.REAPIClient)
	}
	numCPU := runtime.NumCPU()
	if (opts.Limits == Limits{}) {
		opts.Limits = DefaultLimits()
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
	log.Infof("numcpu=%d threads:%d - limits=%#v", numCPU, maxThreads, opts.Limits)

	b := &Builder{
		jobID:     opts.JobID,
		id:        opts.ID,
		projectID: opts.ProjectID,
		metadata:  opts.Metadata,

		path:              opts.Path,
		hashFS:            opts.HashFS,
		start:             start,
		graph:             graph,
		stepSema:          semaphore.New("step", opts.Limits.Step),
		scanDeps:          scandeps.New(opts.HashFS, graph.InputDeps(ctx)),
		localSema:         semaphore.New("localexec", opts.Limits.Local),
		localExec:         le,
		remoteSema:        semaphore.New("remoteexec", opts.Limits.Remote),
		remoteExec:        re,
		reCacheEnableRead: opts.RECacheEnableRead,
		// reCacheEnableWrite: opts.RECacheEnableWrite,
		actionSalt:  opts.ActionSalt,
		reapiclient: opts.REAPIClient,

		outputLocal:     opts.OutputLocal,
		cache:           opts.Cache,
		clobber:         opts.Clobber,
		prepare:         opts.Prepare,
		verbose:         opts.Verbose,
		dryRun:          opts.DryRun,
		failures:        failures{allowed: opts.FailuresAllowed},
		rebuildManifest: opts.RebuildManifest,
	}
	return b, nil
}

// Close cleans up the builder.
func (b *Builder) Close() error {
	return nil
}

// Stats returns stats of the builder.
func (b *Builder) Stats() Stats {
	return b.stats.stats()
}

// ErrManifestModified is an error to indicate that manifest is modified.
var ErrManifestModified = errors.New("manifest modified")

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
			log.Errorf("panic in build: %v\n%s", r, loc)
			log.Warnf("%s", buf)
			if err == nil {
				err = fmt.Errorf("panic in build: %v", r)
			}
		}
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
	sched := newScheduler(schedOpts)
	err = schedule(ctx, sched, b.graph, args...)
	if err != nil {
		return err
	}
	b.plan = sched.plan
	b.stats = newStats(sched.total)

	stat := b.Stats()
	if stat.Total == 0 {
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
	}

	var mftime time.Time
	if b.rebuildManifest != "" {
		fi, err := b.hashFS.Stat(ctx, b.path.ExecRoot, filepath.Join(b.path.Dir, b.rebuildManifest))
		if err == nil {
			mftime = fi.ModTime()
			log.Infof("manifest %s: %s", b.rebuildManifest, mftime)
		}
	}
	defer func() {
		stat = b.Stats()
		if b.rebuildManifest != "" {
			fi, mferr := b.hashFS.Stat(ctx, b.path.ExecRoot, filepath.Join(b.path.Dir, b.rebuildManifest))
			if mferr != nil {
				log.Warnf("failed to stat %s: %v", b.rebuildManifest, mferr)
				return
			}
			if err != nil {
				return
			}
			log.Infof("rebuild manifest %#v %s: %s->%s: %s", stat, b.rebuildManifest, mftime, fi.ModTime(), time.Since(started))
			if fi.ModTime().After(mftime) || stat.Done != stat.Skipped {
				ui.Default.PrintLines(fmt.Sprintf("%6s Regenerating ninja files", ui.FormatDuration(time.Since(started))))
				err = ErrManifestModified
				return
			}
			return
		}
		log.Infof("build %s %s: %v", time.Since(started), time.Since(b.start), err)
		if stat.Skipped == stat.Total {
			ui.Default.PrintLines(ninjaNoWorkToDo)
			return
		}
	}()
	pstat := b.plan.stats()
	b.progress.report("build start: Ready %d Pending %d", pstat.nready, pstat.npendings)
	b.progress.start(ctx, b)
	defer b.progress.stop()

	// prepare all output directories for local process,
	// to minimize mkdir operations.
	// if we create out dirs before each action concurrently,
	// need to check the dir many times and worry about race.
	err = b.prepareAllOutDirs(ctx)
	if err != nil {
		log.Warnf("failed to prepare all out dirs: %v", err)
		return err
	}

	var wg sync.WaitGroup
	var stuck bool
	errch := make(chan error, 1000)

loop:
	for {
		done, err := b.stepSema.WaitAcquire(ctx)
		if err != nil {
			cancel()
			return err
		}

		var step *Step
		var ok bool
		select {
		case step, ok = <-b.plan.q:
			if !ok {
				done()
				break loop
			}
		case err := <-errch:
			done()
			var shouldFail bool
			if err != nil {
				shouldFail = b.failures.shouldFail(err)
			}
			numServs := b.stepSema.NumServs()
			hasReady := b.plan.hasReady()
			// no active steps and no ready steps?
			if !stuck {
				stuck = numServs == 0 && !hasReady
			}
			if shouldFail || stuck {
				cancel()
				break loop
			}
			continue
		case <-ctx.Done():
			done()
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
					log.Errorf("runStep panic: %v\nstep: %s\n%s", r, out, loc)
					log.Warnf("%s", buf)
					err = fmt.Errorf("panic: %v: %s", r, loc)
				}
			}()
			defer done()

			err = b.runStep(ctx, step)
			select {
			case <-ctx.Done():
				return
			default:
			}
		}(step)
	}
	errdone := make(chan error)
	go func() {
		var canceled bool
		for e := range errch {
			b.failures.shouldFail(e)
			if errors.Is(e, context.Canceled) {
				canceled = true
			}
		}
		// step error is already reported in run_step.
		// report errStuck if it coulddn't progress.
		// otherwise, report just number of errors.
		if b.failures.n == 0 {
			if canceled {
				errdone <- context.Cause(ctx)
				return
			}
			errdone <- nil
			return
		}
		if stuck {
			errdone <- fmt.Errorf("cannot make progress due to previous %d errors: %w", b.failures.n, b.failures.firstErr)
			return
		}
		errdone <- fmt.Errorf("%d steps failed: %w", b.failures.n, b.failures.firstErr)
	}()
	wg.Wait()
	close(errch)
	err = <-errdone
	if !b.verbose {
		if err == nil {
			ui.Default.PrintLines(fmt.Sprintf("%s finished", name))
		} else {
			ui.Default.PrintLines(fmt.Sprintf("%s failed", name))
		}
	}
	return err
}

func (b *Builder) uploadBuildNinja(ctx context.Context) {
	inputs := b.graph.Filenames()
	inputs = append(inputs, "args.gn")
	ents, err := b.hashFS.Entries(ctx, filepath.Join(b.path.ExecRoot, b.path.Dir), inputs)
	if err != nil {
		log.Warnf("failed to get build files entries: %v", err)
		return
	}
	ds := digest.NewStore()
	tree := merkletree.New(ds)
	for _, ent := range ents {
		err := tree.Set(ent)
		if err != nil {
			log.Warnf("failed to set %s: %v", ent.Name, err)
		}
	}
	d, err := tree.Build(ctx)
	if err != nil {
		log.Warnf("failed to calculate tree: %v", err)
		return
	}
	_, err = b.reapiclient.UploadAll(ctx, ds)
	if err != nil {
		log.Warnf("failed to upload build files tree %s: %v", d, err)
		return
	}
}

// dedupInputs deduplicates inputs.
func dedupInputs(cmd *execute.Cmd) {
	m := make(map[string]string)
	inputs := make([]string, 0, len(cmd.Inputs))
	for _, input := range cmd.Inputs {
		key := input
		if _, found := m[key]; found {
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
	outputs := step.cmd.Outputs
	if step.def.Binding("phony_outputs") != "" {
		return nil
	}

	if step.cmd.Depfile != "" {
		switch step.cmd.Deps {
		case "gcc":
			// for deps=gcc, ninja will record it in
			// deps log and remove depfile.
		default:
			outputs = append(outputs, step.cmd.Depfile)
		}
	}

	localOutputs := step.def.LocalOutputs(ctx)
	seen := make(map[string]bool)
	for _, o := range localOutputs {
		if seen[o] {
			continue
		}
		seen[o] = true
	}

	defOutputs := step.def.Outputs(ctx)
	// need to check against step.cmd.Outputs, not step.def.Outputs, since
	// handler may add to step.cmd.Outputs.
	for _, out := range outputs {
		if seen[out] {
			continue
		}
		seen[out] = true
		var local bool
		if b.outputLocal != nil && b.outputLocal(ctx, out) {
			localOutputs = append(localOutputs, out)
			local = true
		} else {
			// check if it already exists on local.
			// if so, better to flush to the disk
			// as other future step would access it locally.
			_, err := os.Lstat(filepath.Join(step.cmd.ExecRoot, out))
			if err == nil {
				localOutputs = append(localOutputs, out)
				local = true
			}
		}
		_, err := b.hashFS.Stat(ctx, step.cmd.ExecRoot, out)
		if err != nil {
			reqOut := slices.Contains(defOutputs, out)
			if reqOut {
				return fmt.Errorf("missing outputs %s: %w", out, err)
			}
			log.Warnf("missing outputs %s: %v", out, err)
			if !local {
				// need to make sure it doesn't exist on disk too
				// for local=true, Flush will remove.
				err = os.Remove(filepath.Join(step.cmd.ExecRoot, out))
				if err != nil && !errors.Is(err, fs.ErrNotExist) {
					log.Warnf("remove missing outputs %q: %v", out, err)
				}
			}
			continue
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
func (b *Builder) progressStepCacheHit(step *Step) {
	b.progress.step(b, step, progressPrefixCacheHit+step.cmd.Desc)
}

// progressStepStarted shows progress of the started step.
func (b *Builder) progressStepStarted(step *Step) {
	step.setPhase(stepStart)
	b.progress.step(b, step, progressPrefixStart+step.cmd.Desc)
}

// progressStepFinished shows progress of the finished step.
func (b *Builder) progressStepFinished(step *Step) {
	step.setPhase(stepDone)
	b.progress.step(b, step, progressPrefixFinish+step.cmd.Desc)
}

// progressStepRetry shows progress of the retried step.
func (b *Builder) progressStepRetry(step *Step) {
	b.progress.step(b, step, progressPrefixRetry+step.cmd.Desc)
}

var errNotRelocatable = errors.New("request is not relocatable")

func (b *Builder) updateDeps(ctx context.Context, step *Step) error {
	if len(step.cmd.Outputs) == 0 {
		log.Warnf("update deps: no outputs")
		return nil
	}
	output, err := filepath.Rel(step.cmd.Dir, step.cmd.Outputs[0])
	if err != nil {
		log.Warnf("update deps: failed to get rel %s,%s: %v", step.cmd.Dir, step.cmd.Outputs[0], err)
		return nil
	}
	fi, err := b.hashFS.Stat(ctx, step.cmd.ExecRoot, step.cmd.Outputs[0])
	if err != nil {
		log.Warnf("update deps: missing outputs %s: %v", step.cmd.Outputs[0], err)
		return nil
	}
	deps, err := depsAfterRun(ctx, b, step)
	if err != nil {
		return err
	}
	_, err = step.def.RecordDeps(ctx, output, fi.ModTime(), deps)
	if err != nil {
		log.Warnf("update deps: failed to record deps %s, %s, %s, %s: %v", output, base64.StdEncoding.EncodeToString(step.cmd.CmdHash), fi.ModTime(), deps, err)
	}
	canonicalizedDeps := make([]string, 0, len(deps))
	for _, dep := range deps {
		canonicalizedDeps = append(canonicalizedDeps, b.path.MaybeFromWD(dep))
	}
	depsFixCmd(ctx, b, step, canonicalizedDeps)
	return nil
}

func (b *Builder) prepareAllOutDirs(ctx context.Context) error {
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
	for _, dir := range dirs {
		// we don't use hashfs here for performance.
		// just create dirs on local disk, so local process
		// can detect the dirs.
		err := os.MkdirAll(filepath.Join(b.path.ExecRoot, dir), 0755)
		if err != nil {
			return err
		}
	}
	return nil
}

func (b *Builder) ActiveSteps() []ActiveStepInfo {
	return b.progress.ActiveSteps()
}
