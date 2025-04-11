// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	log "github.com/golang/glog"

	"go.chromium.org/infra/build/siso/hashfs"
	"go.chromium.org/infra/build/siso/o11y/clog"
	"go.chromium.org/infra/build/siso/ui"
)

var (
	// ErrNoTarget is an error when target is not found in the Graph.
	ErrNoTarget = errors.New("target not found")

	// ErrTargetIsSource is an error when target is the source in the Graph.
	ErrTargetIsSource = errors.New("target is source")

	// ErrDuplicateStep is an error when step for the target is already created.
	ErrDuplicateStep = errors.New("duplicate step")

	// ErrMissingDeps is an error when step failed to get deps.
	ErrMissingDeps = errors.New("missing deps in depfile/depslog")

	// ErrStaleDeps is an error when deps log is stale.
	ErrStaleDeps = errors.New("stale deps in depslog")
)

// Target is a build target used in Graph.
// 0 means no target.
// valid range is [0, NumTargets).
type Target int

// Graph provides a build graph, i.e. step definitions.
type Graph interface {
	// NumTargets returns number of valid taraget id.
	NumTargets() int

	// Targets returns target paths for given args.
	// Targets may return known targets even if err != nil.
	Targets(context.Context, ...string) ([]Target, error)

	// SpellcheckTarget returns the most similar target from given target.
	SpellcheckTarget(string) (string, error)

	// Validations returns validation targets detected by past StepDef calls.
	Validations() []Target

	// TargetPath returns exec-root relative path of target.
	TargetPath(context.Context, Target) (string, error)

	// StepDef creates new StepDef for the target and its inputs/orderOnly/outputs targets.
	// if err is ErrTargetIsSource, target is source and no step to
	// generate the target.
	// if err is ErrDuplicateStep, a step that geneartes the target
	// is already processed.
	StepDef(context.Context, Target, StepDef) (StepDef, []Target, []Target, []Target, error)

	// InputDeps returns input dependencies.
	// input dependencies is a map from input path or label to
	// other files or labels needed for the key.
	// path is exec root relative and label contains ':'.
	// it's "input_deps" in Starlark config.
	InputDeps(context.Context) map[string][]string

	// StepLimits returns a map of maximum number of concurrent
	// steps by pool name.
	StepLimits(context.Context) map[string]int

	// Filenames returns filenames of build manifest (all files loaded by build.ninja).
	Filenames() []string
}

// TargetError is an error of unknown target for build.
type TargetError struct {
	err      error
	Suggests []string
}

func (e TargetError) Unwrap() error {
	return e.err
}

func (e TargetError) Error() string {
	return e.err.Error()
}

// MissingSourceError is an error of missing source needed for build.
type MissingSourceError struct {
	Target   string
	NeededBy string
}

func (e MissingSourceError) Error() string {
	if e.NeededBy != "" {
		return fmt.Sprintf("%q, needed by %q, missing and no known rule to make it", e.Target, e.NeededBy)
	}
	return fmt.Sprintf("%q missing and no known rule to make it", e.Target)
}

type scanState int

const (
	scanStateNotVisited scanState = iota
	scanStateVisiting
	scanStateIgnored
	scanStateDone
)

func (s scanState) String() string {
	switch s {
	case scanStateNotVisited:
		return "not-visited"
	case scanStateVisiting:
		return "visiting"
	case scanStateIgnored:
		return "ignored"
	case scanStateDone:
		return "done"
	default:
		return fmt.Sprintf("unknown=%d", int(s))
	}
}

type targetInfo struct {
	// scanState of the target.
	scan scanState
	// list of steps that wait the target becomes ready.
	waits []*Step
	// true if the target is source (no step generates the target).
	source bool
	// true if the target is generated output.
	output bool
	// true if the target is phony_output.
	phonyOutput bool
}

// plan maintains which step to execute next.
type plan struct {
	mu     sync.Mutex
	closed bool
	q      chan *Step // queue for ready to run
	ready  []*Step    // spilled over from q
	// indexed by Target
	targets   []targetInfo
	npendings int
}

// schedulerOption is scheduler option.
type schedulerOption struct {
	NumTargets  int
	Path        *Path
	HashFS      *hashfs.HashFS
	Prepare     bool
	EnableTrace bool
}

// scheduler creates a plan.
type scheduler struct {
	path   *Path
	hashFS *hashfs.HashFS

	plan *plan

	// number of steps scheduled.
	total int

	lastProgress time.Time
	visited      int

	// prepare runs steps to generate inputs for the requested targets,
	// but not run steps to generate requested targets.
	prepare bool

	// prepareHeaderOnly runs steps to generate headers needed for
	// the requested targets.
	prepareHeaderOnly bool

	enableTrace bool
}

func targetPath(ctx context.Context, g Graph, t Target) string {
	p, err := g.TargetPath(ctx, t)
	if err != nil {
		return fmt.Sprint(err)
	}
	return p
}

// schedule schedules build plans for args from graph into sched.
func schedule(ctx context.Context, sched *scheduler, graph Graph, args ...string) error {
	targets, err := graph.Targets(ctx, args...)
	started := time.Now()
	clog.Infof(ctx, "schedule targets: %v [%d]: %v", targets, graph.NumTargets(), err)
	if err != nil {
		if !experiments.Enabled("ignore-missing-targets", "") {
			return TargetError{err: err, Suggests: suggestTargets(ctx, sched, graph, args...)}
		}
		ui.Default.PrintLines(ui.SGR(ui.Yellow, fmt.Sprintf("WARNING: ignore missing targets: %v\n\n", err)))
	}
	if len(targets) == 0 {
		return TargetError{err: errors.New("no targets")}
	}
	if len(args) > 0 {
		var targetNames []string
		for _, t := range targets {
			targetNames = append(targetNames, sched.path.MaybeToWD(ctx, targetPath(ctx, graph, t)))
		}
		if !slices.Equal(args, targetNames) {
			ui.Default.PrintLines(fmt.Sprintf("target: %q\n    ->  %q\n\n", args, targetNames))
		}
	}
	for _, t := range targets {
		switch sched.plan.targets[t].scan {
		case scanStateNotVisited:
		case scanStateVisiting:
			return fmt.Errorf("scan state %q: visiting", targetPath(ctx, graph, t))
		case scanStateDone, scanStateIgnored:
			continue
		}

		err := scheduleTarget(ctx, sched, graph, t, nil, sched.prepare)
		if err != nil {
			return fmt.Errorf("failed in schedule %s: %w", targetPath(ctx, graph, t), err)
		}
	}
	if !sched.prepare {
		for _, t := range graph.Validations() {
			switch sched.plan.targets[t].scan {
			case scanStateNotVisited:
			case scanStateVisiting:
				return fmt.Errorf("scan state %q: visiting", targetPath(ctx, graph, t))
			case scanStateDone, scanStateIgnored:
				continue
			}
			err := scheduleTarget(ctx, sched, graph, t, nil, false)
			if err != nil {
				return fmt.Errorf("failed in schedule %s: %w", targetPath(ctx, graph, t), err)
			}
		}
	}
	sched.finish(ctx, time.Since(started))
	return nil
}

// DependencyCycleError is error type for dependency cycle.
type DependencyCycleError struct {
	Targets []string
}

func (d DependencyCycleError) Error() string {
	return fmt.Sprintf("dependency cycle: %s", strings.Join(d.Targets, " -> "))
}

// scheduleTarget schedules a build plan for target, which is required to next StepDef, from graph into sched.
func scheduleTarget(ctx context.Context, sched *scheduler, graph Graph, target Target, next StepDef, ignore bool) error {
	targets := sched.plan.targets
	scanState := targets[target].scan
	switch scanState {
	case scanStateNotVisited:
		targets[target].scan = scanStateVisiting
		defer func() {
			if ignore {
				targets[target].scan = scanStateIgnored
				clog.Infof(ctx, "scan state ignore target %s", targetPath(ctx, graph, target))
				return
			}
			if log.V(1) {
				clog.Infof(ctx, "scan state done target %s", targetPath(ctx, graph, target))
			}
			targets[target].scan = scanStateDone
		}()
	case scanStateVisiting:
		return DependencyCycleError{
			Targets: []string{targetPath(ctx, graph, target)},
		}
	case scanStateIgnored:
		if ignore {
			return nil
		}
		clog.Infof(ctx, "need to scan ignored target %s", targetPath(ctx, graph, target))
		targets[target].scan = scanStateVisiting
		defer func() {
			targets[target].scan = scanStateDone
		}()

	case scanStateDone:
		return nil
	}
	if targets[target].source {
		if log.V(1) {
			clog.Infof(ctx, "sched target already marked: %v", targetPath(ctx, graph, target))
		}
		return nil
	}
	if log.V(1) {
		clog.Infof(ctx, "schedule target %v state=%v ignore:%t", targetPath(ctx, graph, target), scanState, ignore)
	}
	newStep, inputs, orderOnly, outputs, err := graph.StepDef(ctx, target, next)
	switch {
	case err == nil:
		// need to schedule.
	case errors.Is(err, ErrNoTarget):
		if log.V(1) {
			clog.Infof(ctx, "sched target not found? %v", targetPath(ctx, graph, target))
		}
		return err
	case errors.Is(err, ErrTargetIsSource):
		if log.V(1) {
			clog.Infof(ctx, "sched target is source? %v", targetPath(ctx, graph, target))
		}
		return sched.mark(ctx, graph, target, next)
	case errors.Is(err, ErrDuplicateStep):
		if scanState == scanStateIgnored {
			// need to check again.
			// It was ignored, but now required to generate *.h
			clog.Infof(ctx, "need to sched dupliate step for %s", targetPath(ctx, graph, target))
			break
		}
		// this step is already processed.
		if log.V(1) {
			clog.Infof(ctx, "sched duplicate step for %v", targetPath(ctx, graph, target))
		}
		return nil
	default:
		if log.V(1) {
			clog.Warningf(ctx, "sched error for %v: %v", target, err)
		}
		return err
	}
	// mark all other outputs are done, or ignored.
	defer func() {
		nextState := scanStateDone
		if ignore {
			nextState = scanStateIgnored
		}
		for _, out := range outputs {
			switch targets[out].scan {
			case scanStateNotVisited:
				targets[out].scan = nextState
				if log.V(1) {
					clog.Infof(ctx, "scan state %v other target %s", nextState, targetPath(ctx, graph, out))
				}
			}
		}
	}()
	isPhonyOutput := newStep.Binding("phony_output") != ""
	targets[target].phonyOutput = isPhonyOutput
	if log.V(1) {
		clog.Infof(ctx, "schedule %v inputs:%d outputs:%d", newStep, len(inputs), len(outputs))
	}
	sched.visited++
	next = newStep
	select {
	case <-ctx.Done():
		return fmt.Errorf("interrupted in schedule: %w", context.Cause(ctx))
	default:
	}

	if ignore && sched.prepareHeaderOnly {
		clog.Infof(ctx, "check outputs=%d for %s", len(outputs), targetPath(ctx, graph, target))
		// If this step generates header (even if build dependency
		// doesn't explicitly depend on the header), don't ignore this.
		// b/358693473
	outCheck:
		for _, out := range outputs {
			fname, err := graph.TargetPath(ctx, out)
			if err != nil {
				return fmt.Errorf("schedule bad target %s: %w", targetPath(ctx, graph, out), err)
			}
			switch filepath.Ext(fname) {
			case ".h", ".hxx", ".hpp", ".inc":
				clog.Infof(ctx, "need to schedule for %s", fname)
				ignore = false
				break outCheck
			}
			if log.V(1) {
				clog.Infof(ctx, "schedule %s ignore output=%s", targetPath(ctx, graph, target), fname)
			}

		}
	}
	if log.V(1) {
		clog.Infof(ctx, "target=%s ignore=%t prepareHeaderOnly=%t", targetPath(ctx, graph, target), ignore, sched.prepareHeaderOnly)
	}

	// we might not need to use depfile's dependencies to construct
	// build graph.
	// - if depfile's dependency is source file, the file already exists
	//   so no need to wait for it. doesn't change build graph.
	// - if depfile's dependency is generated file
	//   - and if it is included in step's inputs, or indirect inputs,
	//     then, it just adds redundant edge to build graph. Without
	//     the edge, step's order won't be changed, so no need to add
	//     such edge.
	//   - otherwise, it will change the build graph.
	//     it means original build graph without depfile contains
	//     missing dependencies. It would be better to fix gn/ninja's
	//     build graph, rather than mitigating here in the siso.
	step := &Step{
		def:   newStep,
		state: &stepState{},
	}
	orderOnlyIndex := len(inputs)
	for i, in := range append(inputs, orderOnly...) {
		if targets[in].scan != scanStateDone {
			// if this target is ignored, but "in" is header,
			// then it will not ignore steps to generate "in"
			// and "in"'s inputs recursively.
			var inIgnore bool
			if ignore && sched.prepareHeaderOnly {
				fname, err := graph.TargetPath(ctx, in)
				if err != nil {
					return fmt.Errorf("schedule bad target %s: %w", targetPath(ctx, graph, in), err)
				}
				switch filepath.Ext(fname) {
				case ".h", ".hxx", ".hpp", ".inc":
				default:
					clog.Infof(ctx, "may ignore schedule for %s", fname)
					inIgnore = true
				}
			}
			err := scheduleTarget(ctx, sched, graph, in, next, inIgnore)
			if err != nil {
				var cycleErr DependencyCycleError
				if errors.As(err, &cycleErr) {
					if len(cycleErr.Targets) <= 1 || cycleErr.Targets[0] != cycleErr.Targets[len(cycleErr.Targets)-1] {
						cur := targetPath(ctx, graph, in)
						cycleErr.Targets = append(cycleErr.Targets, cur)
					}
					return cycleErr
				}
				return fmt.Errorf("schedule %s: %w", targetPath(ctx, graph, in), err)
			}
		}
		if !targets[in].source && !ignore {
			// If in is not marked (i.e. source), some step
			// will generate it, so need to wait for it
			// before running this step.
			//
			// If this step is ignored, no need to add the step
			// to in's wait.  Otherwise, ignored step may be
			// needed in other dependency chain, and add step
			// for that case, so step appeared several times
			// in targets[in].waits, which would run the same
			// step multiple time, and would fail with race.
			targets[in].waits = append(targets[in].waits, step)
			step.nwaits++
		}
		if i < orderOnlyIndex && targets[in].phonyOutput && !isPhonyOutput {
			// non-phony_output rule can't depend on phony_output.
			// i.e. step's outputs should be phony outputs
			// phony_output is always dirty,
			// so such non-phony output rule always rebuild.
			return fmt.Errorf("schedule: non phony_output %q depends on phony_output %q", targetPath(ctx, graph, target), targetPath(ctx, graph, in))
		}
	}
	if ignore {
		clog.Infof(ctx, "sched: ignore target %s", targetPath(ctx, graph, target))
		return nil
	}
	if log.V(1) {
		clog.Infof(ctx, "sched: add target %s: %s", targetPath(ctx, graph, target), newStep)
	}
	step.outputs = outputs
	sched.add(ctx, graph, step)
	return nil
}

// newScheduler creates new scheduler.
func newScheduler(ctx context.Context, opt schedulerOption) *scheduler {
	var prepareHeaderOnly bool
	if opt.Prepare {
		clog.Infof(ctx, "schedule: prepare mode")
		if experiments.Enabled("prepare-header-only", "prepare header only") {
			clog.Infof(ctx, "schedule: prepare header only mode")
			prepareHeaderOnly = true
		}
	}
	if opt.EnableTrace {
		clog.Infof(ctx, "schedule: enable trace")
	}
	clog.Infof(ctx, "schedule: new: targets=%d", opt.NumTargets)
	return &scheduler{
		path:   opt.Path,
		hashFS: opt.HashFS,
		plan: &plan{
			// preallocate capacity for performance optimization.
			q:       make(chan *Step, 10000),
			targets: make([]targetInfo, opt.NumTargets),
		},
		prepare:           opt.Prepare,
		prepareHeaderOnly: prepareHeaderOnly,
		enableTrace:       opt.EnableTrace,
	}
}

// mark marks target (exec root relative) as source file.
func (s *scheduler) mark(ctx context.Context, graph Graph, target Target, next StepDef) error {
	fname, err := graph.TargetPath(ctx, target)
	if err != nil {
		return err
	}
	_, err = s.hashFS.Stat(ctx, s.path.ExecRoot, fname)
	if err != nil {
		var neededBy string
		if next != nil && len(next.Outputs(ctx)) > 0 {
			neededBy = s.path.MaybeToWD(ctx, next.Outputs(ctx)[0])
		}
		return MissingSourceError{
			Target:   s.path.MaybeToWD(ctx, fname),
			NeededBy: neededBy,
		}
	}
	s.plan.targets[target].source = true
	return nil
}

func (s *scheduler) progressReport(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	ui.Default.PrintLines(msg)
}

// finish finishes the scheduling.
func (s *scheduler) finish(ctx context.Context, d time.Duration) {
	s.plan.mu.Lock()
	defer s.plan.mu.Unlock()
	nready := len(s.plan.q) + len(s.plan.ready)
	npendings := s.plan.npendings
	clog.Infof(ctx, "schedule finish pending:%d+ready:%d (node:%d edge:%d) in %s", npendings, nready, len(s.plan.targets), s.visited, d)
	if d < ui.DurationThreshold {
		return
	}
	s.progressReport("%6s schedule pending:%d+ready:%d (node:%d edge:%d)\n", ui.FormatDuration(d), npendings, nready, len(s.plan.targets), s.visited)
}

// add adds new stepDef to run.
func (s *scheduler) add(ctx context.Context, graph Graph, step *Step) {
	s.plan.mu.Lock()
	defer s.plan.mu.Unlock()
	defer func() {
		if time.Since(s.lastProgress) < 1*time.Second {
			return
		}
		nready := len(s.plan.q) + len(s.plan.ready)
		npendings := s.plan.npendings
		s.progressReport("schedule pending:%d+ready:%d (node:%d edge:%d)", npendings, nready, len(s.plan.targets), s.visited)
		s.lastProgress = time.Now()
	}()
	s.total++
	if !step.def.IsPhony() {
		// don't add output for phony targets. https://crbug.com/1517575
		for _, output := range step.outputs {
			s.plan.targets[output].output = true
		}
	}
	if step.ReadyToRun("", Target(0)) {
		if log.V(1) {
			clog.Infof(ctx, "step state: %s ready to run", step.String())
		}
		select {
		case s.plan.q <- step:
		default:
			step.queueTime = time.Now()
			step.queueSize = len(s.plan.ready)
			s.plan.ready = append(s.plan.ready, step)
		}
		return
	}
	if log.V(1) {
		clog.Infof(ctx, "pending to run: %s (waits: %d)", step, step.NumWaits())
	}
	s.plan.npendings++
}

type planStats struct {
	npendings int
	nready    int
}

func (p *plan) stats() planStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return planStats{
		npendings: p.npendings,
		nready:    len(p.q) + len(p.ready),
	}
}

func (p *plan) pushReady() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.ready) == 0 {
		return
	}
	select {
	case p.q <- p.ready[0]:
		p.ready[0].queueDuration = time.Since(p.ready[0].queueTime)
		// Deallocate p.ready[0] explicitly.
		copy(p.ready, p.ready[1:])
		p.ready[len(p.ready)-1] = nil
		p.ready = p.ready[:len(p.ready)-1]
	default:
	}
}

func (p *plan) hasReady() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.q) > 0 || len(p.ready) > 0
}

func (p *plan) done(ctx context.Context, step *Step) {
	outs := step.outputs
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		clog.Infof(ctx, "build already finished. nothing triggers")
		return
	}

	// Before processing the completed step,
	// send ready steps from p.ready to p.q and resize p.ready.
	i := 0
	for _, s := range p.ready {
		select {
		case p.q <- s:
			s.queueDuration = time.Since(s.queueTime)
		default:
			p.ready[i] = s
			i++
		}
	}
	for j := i; j < len(p.ready); j++ {
		p.ready[j] = nil
	}
	p.ready = p.ready[:i]

	// Unblock waiting steps and send them to the queue if they are ready.
	npendings := p.npendings
	nready := 0
	ready := make([]*Step, 0, len(outs))
	for _, out := range outs {
		if log.V(1) {
			clog.Infof(ctx, "done %v", out)
		}
		i = 0
		for _, s := range p.targets[out].waits {
			prevNonPhony := step.String()
			if step.def.IsPhony() {
				prevNonPhony = step.prevStepID
			}
			if s.ReadyToRun(prevNonPhony, out) {
				p.npendings--
				nready++
				if log.V(1) {
					clog.Infof(ctx, "step state: %s ready to run %q", s.String(), s.def.Outputs(ctx)[0])
				}
				select {
				case p.q <- s:
				default:
					s.queueTime = time.Now()
					s.queueSize = len(ready)
					ready = append(ready, s)
				}
				continue
			}
			p.targets[out].waits[i] = s
			i++
		}
		for j := i; j < len(p.targets[out].waits); j++ {
			p.targets[out].waits[j] = nil
		}
		if i == 0 {
			p.targets[out].waits = nil
			continue
		}
		p.targets[out].waits = p.targets[out].waits[:i]
	}
	if log.V(1) {
		if nready > 0 {
			clog.Infof(ctx, "trigger %d. pendings %d -> %d", nready, npendings, p.npendings)
		} else {
			clog.Infof(ctx, "zero-trigger outs=%v", outs)
		}
	}
	p.ready = append(p.ready, ready...)
	if len(p.ready) == 0 && p.npendings == 0 && !p.closed {
		p.closed = true
		clog.Infof(ctx, "no step in pending. closing q")
		close(p.q)
	}
}

func (p *plan) dump(ctx context.Context, graph Graph) {
	p.mu.Lock()
	defer p.mu.Unlock()
	clog.Infof(ctx, "queue = %d pendings=%d", len(p.q), p.npendings)
	clog.Infof(ctx, "closed=%t", p.closed)
	var steps []*Step
	seen := make(map[*Step]bool)
	waits := make(map[string]bool)
	ready := make([]string, 0, len(p.ready))
	for _, s := range p.ready {
		ready = append(ready, s.String())
		seen[s] = true
		steps = append(steps, s)
	}
	waitTargets := 0
	clog.Infof(ctx, "ready=%q", ready)
	for node, ti := range p.targets {
		ws := ti.waits
		if len(ws) > 0 {
			waitTargets++
		}
		path, err := graph.TargetPath(ctx, Target(node))
		if err != nil {
			clog.Warningf(ctx, "invalid node %v: %v", node, err)
			continue
		}
		waits[path] = true
		for _, s := range ws {
			if seen[s] {
				continue
			}
			seen[s] = true
			steps = append(steps, s)
		}
	}
	for _, s := range steps {
		for _, o := range s.def.Outputs(ctx) {
			if !waits[o] {
				clog.Infof(ctx, "step %s output:%s no trigger", s, o)
				continue
			}
			delete(waits, o)
		}
	}
	outs := make([]string, 0, len(waits))
	for out := range waits {
		outs = append(outs, out)
	}
	sort.Strings(outs)
	clog.Infof(ctx, "waits=%d no-trigger=%d", waitTargets, len(outs))
	clog.Infof(ctx, "no steps will trigger %q", outs)
}

func suggestTargets(ctx context.Context, sched *scheduler, graph Graph, args ...string) []string {
	rel, err := filepath.Rel(filepath.Join(sched.path.ExecRoot, sched.path.Dir), sched.path.ExecRoot)
	if err != nil {
		clog.Warningf(ctx, "failed to get rel to exec root: %v", err)
		return nil
	}
	var suggests []string
	for _, arg := range args {
		_, err := graph.Targets(ctx, arg)
		if err == nil {
			// this target is ok as is.
			suggests = append(suggests, arg)
			continue
		}
		target := strings.TrimSuffix(arg, "^")
		_, err = sched.hashFS.Stat(ctx, sched.path.ExecRoot, filepath.Join(sched.path.Dir, target))
		if err == nil {
			// just missing ^?
			target := filepath.ToSlash(target) + "^"
			_, err = graph.Targets(ctx, target)
			if err == nil {
				suggests = append(suggests, target)
				continue
			}
		}
		_, err = sched.hashFS.Stat(ctx, sched.path.ExecRoot, target)
		if err == nil {
			// wrong relative dir?
			target := filepath.ToSlash(filepath.Join(rel, target) + "^")
			_, err = graph.Targets(ctx, target)
			if err == nil {
				suggests = append(suggests, target)
				continue
			}
		}
		t, err := graph.SpellcheckTarget(target)
		if err == nil {
			suggests = append(suggests, t)
		}
		t, err = graph.SpellcheckTarget(filepath.Join(rel, target))
		if err == nil {
			suggests = append(suggests, t+"^")
		}
	}
	return suggests
}
