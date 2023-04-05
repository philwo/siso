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

	// UnescapedBinding returns unescaped binding value.
	UnescapedBinding(string) string

	// Inputs returns inputs of the step.
	Inputs(context.Context) []string

	// DepInputs returns inputs via depfile of the step.
	// if depfile is not set, returns nil, nil
	// if depfile or deplog is not found, returns wrapped ErrMissingDeps.
	DepInputs(context.Context) ([]string, error)

	// ToolInputs returns tool inputs of the step.
	// ToolInputs is added to deps inputs.
	ToolInputs(context.Context) []string

	// ExpandCaseSensitives expands filenames to be used on case sensitive fs.
	ExpandCaseSensitives(context.Context, []string) []string

	// ExpandLabels expands labels that contain ':' in the given file list.
	ExpandLabels(context.Context, []string) []string

	// ExpandedInputs returns expanded inputs of the step.
	ExpandedInputs(context.Context) []string

	// RemoteInputs maps file used in remote to file exists on local.
	// path in remote action -> local path
	RemoteInputs() map[string]string

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
	// TODO(b/266518906): make fields private after the migration.
	Def      StepDef
	Nwaits   int
	Cmd      *execute.Cmd
	FastDeps bool

	ReadyTime     time.Time
	PrevStepID    string
	PrevStepOut   string
	QueueTime     time.Time
	QueueSize     int
	QueueDuration time.Duration
	StartTime     time.Time

	Metrics StepMetric

	State *StepState
}

// TODO(b/266518906): make this private after the migration.
type StepState struct {
	mu               sync.Mutex
	phase            StepPhase
	weightedDuration time.Duration
}

func (s *StepState) SetPhase(phase StepPhase) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.phase = phase
}

func (s *StepState) Phase() StepPhase {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.phase
}

func newStep(stepDef StepDef, waits []string) *Step {
	return &Step{
		Def:    stepDef,
		Nwaits: len(waits),
		State:  &StepState{},
	}
}

// NumWaits returns number of waits for the step.
func (s *Step) NumWaits() int {
	return s.Nwaits
}

// ReadyToRun checks whether the step is ready to run
// when prev step's out becomes ready.
func (s *Step) ReadyToRun(prev, out string) bool {
	if out != "" {
		s.Nwaits--
	}
	ready := s.Nwaits == 0
	if ready {
		s.ReadyTime = time.Now()
		s.PrevStepID = prev
		s.PrevStepOut = out
	}
	return ready
}

// String returns id of the step.
func (s *Step) String() string {
	if s.Cmd != nil {
		return s.Cmd.ID
	}
	return s.Def.String()
}

// TODO(b/266518906): make this private after the migration.
type StepPhase int

const (
	stepPhaseNone StepPhase = iota
	stepStart
	stepPreproc
	stepInput
	stepLocalRun
	stepRemoteRun
	stepOutput
	stepDone
)

func (s StepPhase) String() string {
	switch s {
	case stepPhaseNone:
		return "none"
	case stepStart:
		return "start"
	case stepPreproc:
		return "prep"
	case stepInput:
		return "input"
	case stepLocalRun:
		return "local"
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

// SetPhase sets a phase of the step.
func (s *Step) SetPhase(phase StepPhase) {
	s.State.SetPhase(phase)
}

// Phase returns the phase of the step.
func (s *Step) Phase() StepPhase {
	return s.State.Phase()
}

// Done checks the step is done.
func (s *Step) Done() bool {
	return s.State.Phase() == stepDone
}

func (s *Step) addWeightedDuration(d time.Duration) {
	s.State.mu.Lock()
	defer s.State.mu.Unlock()
	if s.State.phase == stepDone {
		return
	}
	s.State.weightedDuration += d
}

func (s *Step) getWeightedDuration() time.Duration {
	s.State.mu.Lock()
	defer s.State.mu.Unlock()
	return s.State.weightedDuration
}

func stepSpanName(stepDef StepDef) string {
	if !strings.HasPrefix(stepDef.ActionName(), "__") {
		return stepDef.ActionName()
	}
	cmd := stepDef.Binding("command")
	// TODO(ukai): need to handle python3.exe case on windows?
	cmd = strings.TrimPrefix(cmd, "python3 ")
	i := strings.Index(cmd, " ")
	if i > 0 {
		cmd = cmd[:i]
	}
	return cmd
}

func stepBacktraces(step *Step) []string {
	var locs []string
	var prev string
	for s := step.Def; s != nil; s = s.Next() {
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

// TODO(b/266518906): make this private after the migration.
func (s *Step) Init(ctx context.Context, b *Builder) {
	ctx, span := trace.NewSpan(ctx, "step-init")
	defer span.Close(nil)
	s.Cmd = newCmd(ctx, b, s.Def)
	clog.Infof(ctx, "cmdhash:%s", hex.EncodeToString(s.Cmd.CmdHash))
}

func newCmd(ctx context.Context, b *Builder, stepDef StepDef) *execute.Cmd {
	cmdline := stepDef.Binding("command")
	rspfileContent := stepDef.Binding("rspfile_content")
	cmd := &execute.Cmd{
		ID:         stepDef.String(),
		Desc:       stepDescription(stepDef),
		ActionName: stepDef.ActionName(),
		Args:       b.argTab.InternSlice(stepDef.Args(ctx)),
		// we don't pass environment variables.
		RSPFile:        b.path.MustFromWD(stepDef.UnescapedBinding("rspfile")),
		RSPFileContent: []byte(rspfileContent),
		CmdHash:        cmdhash(cmdline, rspfileContent),
		ExecRoot:       b.path.ExecRoot, // use step binding?
		Dir:            b.path.Dir,
		Inputs:         stepInputs(ctx, b, stepDef),
		ToolInputs:     stepDef.ToolInputs(ctx),
		Outputs:        stepDef.Outputs(),
		// TODO(b/266518906): enable UseSystemInput
		// UseSystemInput: stepDef.Binding("use_system_input") != "",
		Deps:    stepDef.Binding("deps"),
		Depfile: b.path.MustFromWD(stepDef.UnescapedBinding("depfile")),
		Restat:  stepDef.Binding("restat") != "",

		Pure: stepDef.Pure(),

		HashFS: b.hashFS,

		Platform:        stepDef.Platform(),
		RemoteWrapper:   stepDef.Binding("remote_wrapper"),
		RemoteCommand:   stepDef.Binding("remote_command"),
		RemoteInputs:    stepDef.RemoteInputs(),
		CanonicalizeDir: stepDef.Binding("canonicalize_dir") != "",

		// TODO(b/266518906): enable DoNotCache for read-only client
		// DoNotCache: !b.reCacheEnableWrite,
		// TODO(b/266518906): enable SkipCacheLookup
		// SkipCacheLookup: !b.reCacheEnableRead,
		Timeout:    stepTimeout(ctx, stepDef.Binding("timeout")),
		ActionSalt: b.actionSalt,
	}
	if experiments.Enabled("gvisor", "Force gVisor") {
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

func cmdhash(cmdline, rspfileContent string) []byte {
	h := sha256.New()
	fmt.Fprint(h, cmdline)
	if rspfileContent != "" {
		fmt.Fprint(h, rspfileContent)
	}
	return h.Sum(nil)
}
