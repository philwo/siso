// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"infra/build/siso/execute"
	"infra/build/siso/o11y/clog"
	"infra/build/siso/o11y/trace"
)

// StepDef is a build step definition.
// unless specified, path is execroot relative.
type StepDef interface {
	// String returns id of the step.
	String() string

	// Next returns next step's def.
	Next() StepDef

	// Ensure siso rule is applied
	EnsureRule(context.Context)

	// RuleName returns rule name of the step. empty for no rule.
	RuleName() string

	// ActionName returns action name of the step.
	ActionName() string

	// Args returns command line arguments of the step.
	Args(context.Context) []string

	// TODO(b/266518906): add Env for environment variables for the step
	// i.e. envfile support when `ninja -t msvc -e envfile` is used.

	// IsPhony returns true if the step is phony.
	IsPhony() bool

	// Binding returns binding value.
	Binding(string) string

	// Depfile returns exec-root relative depfile path, or empty if not set.
	Depfile() string

	// Rspfile returns exec-root relative rspfile path, or empty if not set.
	Rspfile() string

	// Inputs returns inputs of the step.
	Inputs(context.Context) []string

	// TriggerInputs returns inputs of the step that would trigger
	// the step's action.  no order-only, but includes deps in deps log.
	TriggerInputs(context.Context) ([]string, error)

	// DepInputs returns inputs via depfile of the step.
	// if depfile is not set, returns nil, nil
	// if depfile or deplog is not found, returns wrapped ErrMissingDeps.
	DepInputs(context.Context) ([]string, error)

	// ToolInputs returns tool inputs of the step.
	// ToolInputs is added to deps inputs.
	ToolInputs(context.Context) []string

	// ExpandCaseSensitives expands filenames to be used on case sensitive fs.
	ExpandCaseSensitives(context.Context, []string) []string

	// ExpandedInputs returns expanded inputs of the step.
	ExpandedInputs(context.Context) []string

	// RemoteInputs maps file used in remote to file exists on local.
	// path in remote action -> local path
	RemoteInputs() map[string]string

	// REProxyConfig returns configuration options for using reproxy.
	REProxyConfig() *execute.REProxyConfig

	// Handle runs a handler for the cmd.
	Handle(context.Context, *execute.Cmd) error

	// Outputs returns outputs of the step.
	Outputs() []string

	// LocalOutputs returns outputs of the step that should be written to the local disk.
	LocalOutputs() []string

	// Pure indicates the step is pure or not.
	Pure() bool

	// Platform returns platform properties for remote execution.
	Platform() map[string]string

	// RecordDeps records deps.
	RecordDeps(context.Context, string, time.Time, []string) (bool, error)

	// RuleFix returns required fix for the rule of the step.
	RuleFix(ctx context.Context, inadds, outadds []string) []byte
}

// Step is a build step.
type Step struct {
	def     StepDef
	nwaits  int
	outputs []Target

	cmd *execute.Cmd

	readyTime     time.Time
	prevStepID    string
	prevStepOut   Target
	queueTime     time.Time
	queueSize     int
	queueDuration time.Duration
	startTime     time.Time

	metrics StepMetric

	state *stepState
}

type stepState struct {
	mu               sync.Mutex
	phase            stepPhase
	weightedDuration time.Duration
}

func (s *stepState) SetPhase(phase stepPhase) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.phase = phase
}

func (s *stepState) Phase() stepPhase {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.phase
}

func newStep(stepDef StepDef, numWaits int, outputs []Target) *Step {
	return &Step{
		def:     stepDef,
		nwaits:  numWaits,
		outputs: outputs,
		state:   &stepState{},
	}
}

// NumWaits returns number of waits for the step.
func (s *Step) NumWaits() int {
	return s.nwaits
}

// ReadyToRun checks whether the step is ready to run
// when prev step's out becomes ready.
func (s *Step) ReadyToRun(prev string, out Target) bool {
	if out != nil {
		s.nwaits--
	}
	ready := s.nwaits == 0
	if ready {
		s.readyTime = time.Now()
		s.prevStepID = prev
		s.prevStepOut = out
	}
	return ready
}

// String returns id of the step.
func (s *Step) String() string {
	if s.cmd != nil {
		return s.cmd.ID
	}
	return s.def.String()
}

type stepPhase int

const (
	stepPhaseNone stepPhase = iota
	stepStart
	stepHandler
	stepPreproc
	stepInput
	stepLocalRun
	stepREWrapperRun
	stepRemoteRun
	stepOutput
	stepDone
)

func (s stepPhase) String() string {
	switch s {
	case stepPhaseNone:
		return "none"
	case stepStart:
		return "start"
	case stepHandler:
		return "handler"
	case stepPreproc:
		return "prep"
	case stepInput:
		return "input"
	case stepLocalRun:
		return "local"
	case stepREWrapperRun:
		return "rewrap"
	case stepRemoteRun:
		return "remote"
	case stepOutput:
		return "output"
	case stepDone:
		return "done"
	default:
		return "unknown"
	}
}

// setPhase sets a phase of the step.
func (s *Step) setPhase(phase stepPhase) {
	s.state.SetPhase(phase)
}

// phase returns the phase of the step.
func (s *Step) phase() stepPhase {
	return s.state.Phase()
}

// Done checks the step is done.
func (s *Step) Done() bool {
	return s.state.Phase() == stepDone
}

func (s *Step) addWeightedDuration(d time.Duration) {
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	if s.state.phase == stepDone {
		return
	}
	s.state.weightedDuration += d
}

func (s *Step) getWeightedDuration() time.Duration {
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	return s.state.weightedDuration
}

func stepSpanName(stepDef StepDef) string {
	if !strings.HasPrefix(stepDef.ActionName(), "__") {
		return stepDef.ActionName()
	}
	cmd := stepDef.Binding("command")
	i := strings.Index(cmd, " ")
	if i > 0 {
		// use python script as step name, not python binary itself.
		arg0 := cmd[:i]
		if strings.HasSuffix(strings.TrimSuffix(arg0, ".exe"), "python3") {
			cmd = cmd[i+1:]
		}
	}
	i = strings.Index(cmd, " ")
	if i > 0 {
		cmd = cmd[:i]
	}
	return cmd
}

func stepBacktraces(step *Step) []string {
	var locs []string
	var prev string
	for s := step.def; s != nil; s = s.Next() {
		outs := s.Outputs()
		loc := stepSpanName(s)
		if len(outs) > 0 {
			out := outs[0]
			if odir := filepath.Dir(out); odir != "." {
				out = odir
			}
			loc = fmt.Sprintf("%s %s", loc, out)
		}
		if loc != prev {
			locs = append(locs, loc)
			prev = loc
		}
	}
	return locs
}

// useReclient returns true if the step uses Reclient via rewrapper or reproxy.
// A step with reclient doesn't need to collect dependencies and check action result caches on Siso side.
func (s *Step) useReclient() bool {
	return s.def.Binding("use_remote_exec_wrapper") != "" || s.cmd.REProxyConfig != nil
}

func (s *Step) init(ctx context.Context, b *Builder) {
	ctx, span := trace.NewSpan(ctx, "step-init")
	defer span.Close(nil)
	s.def.EnsureRule(ctx)
	s.cmd = newCmd(ctx, b, s.def)
	clog.Infof(ctx, "cmdhash:%s", hex.EncodeToString(s.cmd.CmdHash))
}

func newCmd(ctx context.Context, b *Builder, stepDef StepDef) *execute.Cmd {
	cmdline := stepDef.Binding("command")
	rspfileContent := stepDef.Binding("rspfile_content")

	outputs := stepDef.Outputs()
	// add build.ninja as outputs of gn step.
	// gn uses
	//
	//  build build.ninja.stamp: gn
	//    generator = 1
	//    depfile = build.ninja.d
	//  build build.ninja: phony build.ninja.stamp
	//    generator = 1
	//
	// so, step "gn" has build.ninja.stamp as output, but
	// not build.ninja. but it generates build.ninja
	// so add it as output to make timestamp of build.ninja
	// correctly managed by Siso.
	// This workaround is needed to make second build as null build.
	if stepDef.ActionName() == "gn" && len(outputs) == 1 && filepath.Base(outputs[0]) == "build.ninja.stamp" {
		outputs = append(outputs, b.path.MustFromWD("build.ninja"))
	}

	cmd := &execute.Cmd{
		ID:         stepDef.String(),
		Desc:       stepDescription(stepDef),
		ActionName: stepDef.ActionName(),
		Args:       b.argTab.InternSlice(stepDef.Args(ctx)),
		// we don't pass environment variables.
		RSPFile:        stepDef.Rspfile(),
		RSPFileContent: []byte(rspfileContent),
		CmdHash:        calculateCmdHash(cmdline, rspfileContent),
		ExecRoot:       b.path.ExecRoot, // use step binding?
		Dir:            b.path.Dir,
		Inputs:         stepInputs(ctx, b, stepDef),
		ToolInputs:     stepDef.ToolInputs(ctx),
		Outputs:        outputs,
		// TODO(b/266518906): enable UseSystemInput
		// UseSystemInput: stepDef.Binding("use_system_input") != "",
		Deps:    stepDef.Binding("deps"),
		Depfile: stepDef.Depfile(),
		Restat:  stepDef.Binding("restat") != "",

		Pure: stepDef.Pure(),

		HashFS: b.hashFS,

		Platform:      stepDef.Platform(),
		RemoteWrapper: stepDef.Binding("remote_wrapper"),
		RemoteCommand: stepDef.Binding("remote_command"),
		RemoteInputs:  stepDef.RemoteInputs(),
		// always copy REProxyConfig, allows safe mutation via cmd.action.fix.
		REProxyConfig:   stepDef.REProxyConfig().Copy(),
		CanonicalizeDir: stepDef.Binding("canonicalize_dir") != "",

		// TODO(b/266518906): enable DoNotCache for read-only client
		// DoNotCache: !b.reCacheEnableWrite,
		SkipCacheLookup: !b.reCacheEnableRead,
		Timeout:         stepTimeout(ctx, stepDef.Binding("timeout")),
		ActionSalt:      b.actionSalt,
	}
	if stepDef.Binding("pool") == "console" {
		// pool=console needs to attach stdin/stdout/stderr
		// so run locally.
		cmd.Platform = nil
		cmd.RemoteWrapper = ""
		cmd.RemoteCommand = ""
		cmd.RemoteInputs = nil
		cmd.REProxyConfig = nil
	} else if experiments.Enabled("gvisor", "Force gVisor") {
		if len(cmd.Platform) == 0 {
			cmd.Platform = map[string]string{}
		}
		cmd.Platform["dockerRuntime"] = "runsc"
	}
	return cmd
}

func stepTimeout(ctx context.Context, d string) time.Duration {
	if d == "" {
		return 0
	}
	dur, err := time.ParseDuration(d)
	if err != nil {
		clog.Warningf(ctx, "failed to parse duration %q: %v", d, err)
		return 0
	}
	return dur
}

func stepInputs(ctx context.Context, b *Builder, stepDef StepDef) []string {
	seen := make(map[string]bool)
	var inputs []string
	for _, in := range stepDef.Inputs(ctx) {
		if seen[in] {
			continue
		}
		seen[in] = true
		inputs = append(inputs, in)
	}
	deps, err := stepDef.DepInputs(ctx)
	if err != nil {
		return inputs
	}
	for _, in := range deps {
		if seen[in] {
			continue
		}
		seen[in] = true
		inputs = append(inputs, in)
	}
	return inputs
}

func stepDescription(stepDef StepDef) string {
	s := stepDef.Binding("description")
	if s != "" {
		return s
	}
	return stepDef.Binding("command")
}

func calculateCmdHash(cmdline, rspfileContent string) []byte {
	h := sha256.New()
	fmt.Fprint(h, cmdline)
	if rspfileContent != "" {
		fmt.Fprint(h, rspfileContent)
	}
	return h.Sum(nil)
}
