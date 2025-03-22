// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/charmbracelet/log"

	"go.chromium.org/infra/build/siso/execute"
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

	// IsPhony returns true if the step is phony.
	IsPhony() bool

	// Binding returns binding value.
	Binding(string) string

	// Depfile returns exec-root relative depfile path, or empty if not set.
	Depfile(context.Context) string

	// Rspfile returns exec-root relative rspfile path, or empty if not set.
	Rspfile(context.Context) string

	// Inputs returns inputs of the step.
	Inputs(context.Context) []string

	// TriggerInputs returns inputs of the step that would trigger
	// the step's action.  no order-only.
	// For  deps in deps log, use DepInputs.
	TriggerInputs(context.Context) []string

	// DepInputs returns iterator for inputs via depfile of the step.
	// if depfile is not set, returns emptyIter, nil
	// if depfile or deplog is not found, returns wrapped ErrMissingDeps.
	// TODO: use iter.Seq[string] in go 1.23
	DepInputs(context.Context) (func(yield func(string) bool), error)

	// ToolInputs returns tool inputs of the step.
	// ToolInputs is added to deps inputs.
	ToolInputs(context.Context) []string

	// ExpandedCaseSensitives returns expanded filenames if platform is case-sensitive.
	ExpandedCaseSensitives(context.Context, []string) []string

	// ExpandedInputs returns expanded inputs of the step.
	ExpandedInputs(context.Context) []string

	// RemoteInputs maps file used in remote to file exists on local.
	// path in remote action -> local path
	RemoteInputs() map[string]string

	// REProxyConfig returns configuration options for using reproxy.
	REProxyConfig() *execute.REProxyConfig

	// CheckInputDeps checks dep can be found in its direct/indirect inputs.
	// Returns true if it is unknown bad deps, false otherwise.
	CheckInputDeps(context.Context, []string) (bool, error)

	// Handle runs a handler for the cmd.
	Handle(context.Context, *execute.Cmd) error

	// Outputs returns outputs of the step.
	Outputs(context.Context) []string

	// LocalOutputs returns outputs of the step that should be written to the local disk.
	LocalOutputs(context.Context) []string

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

	// startTime is the time that the step starts running in a dedicated goroutine.
	// There might be other semaphore throttling for local exec, scandeps, remote exec.
	startTime time.Time

	// endTime is the time that the step ends.
	endTime time.Time

	metrics StepMetric

	state *stepState
}

type stepState struct {
	mu               sync.Mutex
	phase            stepPhase
	weightedDuration time.Duration

	// start time to wait the step to serv.
	// i.e. local semaphore, remote semaphore.
	waitStart time.Time

	// accumulated wait duration in the step.
	waitDuration time.Duration
}

func (s *stepState) SetPhase(phase stepPhase) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.phase = phase
	switch phase {
	case stepLocalWait, stepREWrapperWait, stepRemoteWait, stepFallbackWait, stepRetryWait:
		s.waitStart = time.Now()
	default:
		if !s.waitStart.IsZero() {
			s.waitDuration += time.Since(s.waitStart)
		}
		s.waitStart = time.Time{}
	}
}

func (s *stepState) Phase() stepPhase {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.phase
}

func (s *stepState) WaitDuration() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	dur := s.waitDuration
	if !s.waitStart.IsZero() {
		dur += time.Since(s.waitStart)
	}
	return dur
}

// NumWaits returns number of waits for the step.
func (s *Step) NumWaits() int {
	return s.nwaits
}

// ReadyToRun checks whether the step is ready to run
// when prev step's out becomes ready.
func (s *Step) ReadyToRun(prev string, out Target) bool {
	if out != 0 {
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
	return s.def.String()
}

type stepPhase int

const (
	stepPhaseNone stepPhase = iota
	stepStart
	stepHandler
	stepPreproc
	stepInput
	stepLocalWait
	stepLocalRun
	stepREWrapperWait
	stepREWrapperRun
	stepRemoteWait
	stepRemoteRun
	stepFallbackWait
	stepFallbackRun
	stepRetryWait
	stepRetryRun
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
	case stepLocalWait:
		return "wait-local"
	case stepLocalRun:
		return "local"
	case stepREWrapperWait:
		return "wait-rewrap"
	case stepREWrapperRun:
		return "rewrap"
	case stepRemoteWait:
		return "wait-remote"
	case stepRemoteRun:
		return "remote"
	case stepFallbackWait:
		return "wait-fallback"
	case stepFallbackRun:
		return "fallback"
	case stepRetryWait:
		return "wait-retry"
	case stepRetryRun:
		return "retry"
	case stepOutput:
		return "output"
	case stepDone:
		return "done"
	default:
		return "unknown"
	}
}

func (s stepPhase) wait() stepPhase {
	switch s {
	case stepLocalRun:
		return stepLocalWait
	case stepREWrapperRun:
		return stepREWrapperWait
	case stepRemoteRun:
		return stepRemoteWait
	case stepFallbackRun:
		return stepFallbackWait
	case stepRetryRun:
		return stepRetryWait
	default:
		return s
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

func (s *Step) servDuration() time.Duration {
	return time.Since(s.startTime) - s.state.WaitDuration()
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

// useReclient returns true if the step uses Reclient via rewrapper or reproxy.
// A step with reclient doesn't need to collect dependencies and check action result caches on Siso side.
func (s *Step) useReclient() bool {
	return s.def.Binding("use_remote_exec_wrapper") != "" || s.cmd.REProxyConfig != nil
}

func (s *Step) init(ctx context.Context, b *Builder) {
	s.def.EnsureRule(ctx)
	s.cmd = newCmd(ctx, b, s.def)
	log.Infof("cmdhash:%s", base64.StdEncoding.EncodeToString(s.cmd.CmdHash))
}

func newCmd(ctx context.Context, b *Builder, stepDef StepDef) *execute.Cmd {
	cmdline := stepDef.Binding("command")
	rspfileContent := stepDef.Binding("rspfile_content")

	outputs := stepDef.Outputs(ctx)
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
		outputs = append(outputs, b.path.MaybeFromWD("build.ninja"))
	}

	cmd := &execute.Cmd{
		ID:             stepDef.String(),
		Desc:           stepDescription(stepDef),
		ActionName:     stepDef.ActionName(),
		Args:           b.argTab.InternSlice(stepDef.Args(ctx)),
		RSPFile:        stepDef.Rspfile(ctx),
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
		Depfile: stepDef.Depfile(ctx),

		Restat:        stepDef.Binding("restat") != "",
		RestatContent: stepDef.Binding("restat_content") != "",

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
		Timeout:         stepTimeout(stepDef.Binding("timeout")),
		ActionSalt:      b.actionSalt,
	}
	if envfile := stepDef.Binding("envfile"); envfile != "" {
		cmd.Env = b.loadEnvfile(ctx, envfile)
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
	cmd.InitOutputs()
	return cmd
}

func stepTimeout(d string) time.Duration {
	const defaultTimeout = 1 * time.Hour
	if d == "" {
		return defaultTimeout
	}
	dur, err := time.ParseDuration(d)
	if err != nil {
		log.Warnf("failed to parse duration %q: %v", d, err)
		return defaultTimeout
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
	depsIter, err := stepDef.DepInputs(ctx)
	if err != nil {
		return inputs
	}
	depsIter(func(in string) bool {
		if seen[in] {
			return true
		}
		seen[in] = true
		inputs = append(inputs, in)
		return true
	})
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

func validateRemoteActionResult(result *rpb.ActionResult) bool {
	if result == nil {
		return false
	}

	// Succeeded result should have at least one output. b/350360391
	if result.ExitCode == 0 && len(result.GetOutputFiles()) == 0 {
		return false
	}

	return true
}

type envfile struct {
	once sync.Once
	envs []string
}

func (b *Builder) loadEnvfile(ctx context.Context, fname string) []string {
	env := &envfile{}
	v, loaded := b.envFiles.LoadOrStore(fname, env)
	if loaded {
		env = v.(*envfile)
	}
	env.once.Do(func() {
		// https://ninja-build.org/manual.html#_extra_tools
		// ninja -t msvc -e ENVFILE -- cl.exe <arguments>
		//  Where ENVFILE is a binary file that contains an environment block suitable for CreateProcessA() on Windows (i.e. a series of zero-terminated strings that look like NAME=VALUE, followed by an extra zero terminator).
		buf, err := b.hashFS.ReadFile(ctx, b.path.ExecRoot, b.path.MaybeFromWD(fname))
		if err != nil {
			log.Warnf("failed to load envfile %q: %v", fname, err)
			return
		}
		for len(buf) > 0 {
			i := bytes.IndexByte(buf, '\000')
			if i < 0 {
				env.envs = append(env.envs, string(buf))
				break
			}
			e := string(buf[:i])
			buf = buf[i+1:]
			if e != "" {
				env.envs = append(env.envs, e)
			}
		}
		log.Infof("load envfile %q: %d", fname, len(env.envs))
	})
	return env.envs
}
