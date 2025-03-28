// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjabuild

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/logging"
	log "github.com/golang/glog"
	"github.com/google/uuid"

	"infra/build/siso/build"
	"infra/build/siso/execute"
	"infra/build/siso/o11y/clog"
	"infra/build/siso/o11y/trace"
	"infra/build/siso/toolsupport/cmdutil"
	"infra/build/siso/toolsupport/makeutil"
	"infra/build/siso/toolsupport/ninjautil"
	"infra/build/siso/toolsupport/shutil"
)

// StepDef is a ninja build step definition.
type StepDef struct {
	id   string
	edge *ninjautil.Edge
	next build.StepDef
	rule StepRule
	pure bool

	// from depfile/depslog
	deps   []string // exec root relative
	deperr error

	envfile string // for ninja -t msvc -e <envfile> --

	globals *globals
}

func logFormat(e logging.Entry) string {
	stepID := e.Labels["id"]
	if e.HTTPRequest != nil {
		return fmt.Sprintf("%s %v %s", stepID, e.Payload, e.HTTPRequest.Latency)
	}
	if stepID == "" {
		return fmt.Sprintf("%v", e.Payload)
	}
	return fmt.Sprintf("%s %v", stepID, e.Payload)
}

func (g *Graph) newStepDef(ctx context.Context, edge *ninjautil.Edge, next build.StepDef) *StepDef {
	id := uuid.New().String()
	var rule StepRule
	var pure bool
	if !edge.IsPhony() {
		ctx = clog.NewSpan(ctx, "", "", map[string]string{
			"id": id,
		})
		logger := clog.FromContext(ctx)
		if logger != nil {
			logger.Formatter = logFormat
		}
		rule, pure = g.globals.stepConfig.Lookup(ctx, g.globals.path, edge)
		if log.V(1) {
			clog.Infof(ctx, "rule:%t", pure)
		}
	}
	stepDef := &StepDef{
		id:      id,
		edge:    edge,
		next:    next,
		rule:    rule,
		pure:    pure,
		globals: g.globals,
	}
	updateNodeAssoc(stepDef)
	return stepDef
}

func updateNodeAssoc(stepDef *StepDef) {
	outputs := stepDef.Outputs()
	var solibs []string
	for _, in := range edgeSolibs(stepDef.edge) {
		in = stepDef.globals.path.MustFromWD(in)
		solibs = append(solibs, in)
	}
	if len(solibs) > 0 {
		for _, out := range outputs {
			stepDef.globals.accumulates[out] = append(stepDef.globals.accumulates[out], solibs...)
		}
	}
	if len(outputs) > 0 {
		// associates additional outputs to main output.
		// so step depends on the main output of this step can access
		// additional outputs of this step (in local run).
		out := outputs[0]
		stepDef.globals.accumulates[out] = append(stepDef.globals.accumulates[out], outputs[1:]...)
	}
	if stepDef.IsPhony() {
		seen := make(map[string]bool)
		var inputs []string
		for _, in := range stepDef.edge.Inputs() {
			p := stepDef.globals.path.MustFromWD(in.Path())
			if seen[p] {
				continue
			}
			seen[p] = true
			inputs = append(inputs, p)
		}
		for _, out := range outputs {
			stepDef.globals.replaces[out] = inputs
		}
	}
	if !stepDef.pure {
		return
	}
	var inputs []string
	if stepDef.rule.Replace || stepDef.rule.Accumulate {
		seen := make(map[string]bool)
		for _, in := range stepDef.edge.Inputs() {
			p := stepDef.globals.path.MustFromWD(in.Path())
			if seen[p] {
				continue
			}
			seen[p] = true
			inputs = append(inputs, p)
		}
	}
	if stepDef.rule.Replace {
		for _, out := range outputs {
			stepDef.globals.replaces[out] = inputs
		}
	}
	if stepDef.rule.Accumulate {
		for _, out := range outputs {
			stepDef.globals.accumulates[out] = append(stepDef.globals.accumulates[out], inputs...)
		}
	}
}

// String returns step id.
func (s *StepDef) String() string {
	if s == nil {
		return ""
	}
	return s.id
}

// Next returns next step def.
func (s *StepDef) Next() build.StepDef {
	if s == nil {
		return nil
	}
	if s.next == nil {
		return nil
	}
	return s.next
}

// RuleName returns rule name of the step.
func (s *StepDef) RuleName() string {
	return s.rule.Name
}

// ActionName returns action name of the step.
func (s *StepDef) ActionName() string {
	return s.edge.Rule().Name()
}

// Args returns command line arguments of the step.
func (s *StepDef) Args(ctx context.Context) []string {
	args := stepArgs(s.edge)
	if len(args) > 3 && args[0] == "ninja" && args[1] == "-t" && args[2] == "msvc" {
		flagSet := flag.NewFlagSet("ninja-msvc", flag.ContinueOnError)
		tool := "msvc"
		flagSet.StringVar(&tool, "t", tool, "ninja tool name")
		flagSet.StringVar(&s.envfile, "e", s.envfile, "load environment block from ENVFILE as environment")
		// -o FILE and -p PREFIX is not used?
		err := flagSet.Parse(args[1:])
		if err != nil {
			clog.Warningf(ctx, "%s failed to parse ninja flags %q: %v", s, args, err)
			return args
		}
		if tool != "msvc" {
			return args
		}
		log.V(1).Infof("%s envfile=%q", s, s.envfile)
		return flagSet.Args()
	}
	return args
}

func stepArgs(edge *ninjautil.Edge) []string {
	cmdline := edge.Binding("command")
	if runtime.GOOS == "windows" {
		args, err := cmdutil.Split(cmdline)
		if err != nil {
			return []string{"cmd.exe", "/C", cmdline}
		}
		return args
	}
	args, err := shutil.Split(cmdline)
	if err != nil {
		return []string{"/bin/sh", "-c", cmdline}
	}
	return args
}

// IsPhony returns whether the step if phony or not.
func (s *StepDef) IsPhony() bool {
	return s.edge.IsPhony()
}

// Binding returns a binding of the step.
//
// Ninja bindings are explained in https://ninja-build.org/manual.html#ref_rule:~:text=bindings
// StepDef may overwrites Ninja bindings. e.g. deps, restat.
// StepDef also has custom bindings. e.g. remote_wrapper, remote_command.
func (s *StepDef) Binding(name string) string {
	switch name {
	case "deps":
		if s.rule.Deps == "none" {
			return ""
		}
		if s.rule.Deps == "depfile" && s.edge.Binding("depfile") == "" {
			return ""
		}
		if s.rule.Deps != "" {
			return s.rule.Deps
		}
		return s.edge.Binding(name)
	case "remote_wrapper":
		return s.rule.RemoteWrapper
	case "remote_command":
		return s.rule.RemoteCommand
	case "canonicalize_dir":
		if s.rule.CanonicalizeDir {
			return "true"
		}
		return ""
	case "envfile":
		return s.envfile
	case "use_remote_exec_wrapper":
		if s.rule.UseRemoteExecWrapper {
			return "true"
		}
		return ""
	case "use_system_input":
		if s.rule.UseSystemInput {
			return "true"
		}
		return ""
	case "ignore_extra_input_pattern":
		return s.rule.IgnoreExtraInputPattern
	case "ignore_extra_output_pattern":
		return s.rule.IgnoreExtraOutputPattern
	case "restat":
		if s.rule.Restat {
			return "true"
		}
		return s.edge.Binding(name)
	case "impure":
		if s.rule.Impure {
			return "true"
		}
		return ""
	case "timeout":
		return s.rule.Timeout
	}
	return s.edge.Binding(name)
}

// UnescapedBinding returns unescaped binding.
func (s *StepDef) UnescapedBinding(name string) string {
	return s.edge.UnescapedBinding(name)
}

func edgeSolibs(edge *ninjautil.Edge) []string {
	solibsStr := edge.Binding("solibs")
	if solibsStr == "" {
		return nil
	}
	var solibs []string
	for _, in := range strings.Split(solibsStr, " ") {
		in = strings.TrimSpace(in)
		if in == "" {
			continue
		}
		solibs = append(solibs, in)
	}
	return solibs
}

// Inputs returns inputs of the step.
func (s *StepDef) Inputs(ctx context.Context) []string {
	ctx, span := trace.NewSpan(ctx, "stepdef-inputs")
	defer span.Close(nil)
	seen := make(map[string]bool)
	var targets []string
	for _, in := range s.edge.Inputs() {
		p := in.Path()
		p = s.globals.path.MustFromWD(p)
		if seen[p] {
			continue
		}
		seen[p] = true
		targets = append(targets, p)
	}
	for _, p := range edgeSolibs(s.edge) {
		clog.Infof(ctx, "solib %s", p)
		p = s.globals.path.MustFromWD(p)
		if seen[p] {
			continue
		}
		seen[p] = true
		targets = append(targets, p)
	}
	targets = fixInputs(ctx, s, targets, s.rule.ExcludeInputPatterns)
	targets = append(targets, s.ToolInputs(ctx)...)
	if s.rule.Debug {
		clog.Infof(ctx, "targets=%q", targets)
	}
	return uniqueFiles(targets)
}

// TriggerInputs returns inputs of the step that would trigger the step's action.
func (s *StepDef) TriggerInputs(ctx context.Context) ([]string, error) {
	seen := make(map[string]bool)
	var targets []string
	for _, in := range s.edge.TriggerInputs() {
		p := in.Path()
		p = s.globals.path.MustFromWD(p)
		if seen[p] {
			continue
		}
		seen[p] = true
		targets = append(targets, p)
	}
	deps, err := s.DepInputs(ctx)
	if err != nil {
		return targets, err
	}
	for _, in := range deps {
		if seen[in] {
			continue
		}
		seen[in] = true
		targets = append(targets, in)
	}
	return targets, nil
}

// DepInputs returns inputs stored in depfile / depslog.
func (s *StepDef) DepInputs(ctx context.Context) ([]string, error) {
	ctx, span := trace.NewSpan(ctx, "stepdef-dep-inputs")
	defer span.Close(nil)
	if len(s.deps) == 0 && s.deperr == nil {
		s.deps, s.deperr = depInputs(ctx, s)
	}
	return s.deps, s.deperr
}

// depInputs returns deps inputs of the step.
func depInputs(ctx context.Context, s *StepDef) ([]string, error) {
	var deps []string
	var err error
	switch s.edge.Binding("deps") {
	case "gcc", "msvc":
		// deps info is stored in deps log.
		outputs := s.edge.Outputs()
		if len(outputs) == 0 {
			return nil, fmt.Errorf("%w: no outputs", build.ErrMissingDeps)
		}
		out := outputs[0].Path()
		deps, _, err = s.globals.depsLog.Get(ctx, out)
		if err != nil {
			return nil, fmt.Errorf("%w: failed to lookup deps log %s: %v", build.ErrMissingDeps, out, err)
		}
		clog.Infof(ctx, "depslog %s: %d", out, len(deps))

	case "":
		// deps info is in depfile
		depfile := s.edge.UnescapedBinding("depfile")
		if depfile == "" {
			return nil, nil
		}
		df := s.globals.path.MustFromWD(depfile)
		if s.edge.Binding("generator") != "" {
			// e.g. rule gn.
			// generator runs locally, so believe a local file
			// rather than a file in hashfs.
			s.globals.hashFS.Forget(ctx, s.globals.path.ExecRoot, []string{df})
		}
		_, err := s.globals.hashFS.Stat(ctx, s.globals.path.ExecRoot, df)
		if err != nil {
			return nil, fmt.Errorf("%w: no depfile %s: %v", build.ErrMissingDeps, depfile, err)
		}
		fsys := s.globals.hashFS.FileSystem(ctx, s.globals.path.ExecRoot)
		deps, err = makeutil.ParseDepsFile(ctx, fsys, df)
		if err != nil {
			return nil, fmt.Errorf("%w: failed to load depfile %s: %v", build.ErrMissingDeps, df, err)
		}
		clog.Infof(ctx, "depfile %s: %d", depfile, len(deps))
	}
	var inputs []string
	for _, in := range deps {
		rin := s.globals.path.MustFromWD(in)
		inputs = append(inputs, rin)
	}
	return inputs, nil
}

// ToolInputs returns tool inputs of the step.
func (s *StepDef) ToolInputs(ctx context.Context) []string {
	ctx, span := trace.NewSpan(ctx, "stepdef-tool-inputs")
	defer span.Close(nil)

	var inputs []string
	for _, in := range s.rule.Inputs {
		inputs = append(inputs, fromConfigPath(s.globals.path, in))
	}
	return s.globals.stepConfig.ExpandInputs(ctx, s.globals.path, s.globals.hashFS, inputs)
}

func fixInputs(ctx context.Context, stepDef *StepDef, inputs, excludes []string) []string {
	if stepDef.rule.Debug {
		clog.Infof(ctx, "fix inputs=%d excludes=%d", len(inputs), len(excludes))
	}
	newInputs := make([]string, 0, len(inputs))
	for _, in := range inputs {
		if stepDef.globals.phony[in] {
			clog.Infof(ctx, "inputs %s is phony", in)
			continue
		}
		newInputs = append(newInputs, in)
	}
	inputs = newInputs
	if len(excludes) == 0 {
		return inputs
	}
	rm := make(map[string]bool)
	for _, e := range excludes {
		var m func(in string) bool
		if !strings.Contains(e, "/") {
			// Most exclude_input_patterns are "*.stamp" or so.
			// Just use strings.HasSuffix for such special case.
			if strings.HasPrefix(e, "*") {
				suffix := strings.TrimPrefix(e, "*")
				if !strings.ContainsAny(suffix, `*?[\`) {
					// special case just suffix match.
					m = func(in string) bool {
						return strings.HasSuffix(in, suffix)
					}
				}
			}
			if m == nil {
				// no "/", but has meta character of file name pattern, so use filepath.Match for basename.
				m = func(in string) bool {
					ok, _ := filepath.Match(e, filepath.Base(in))
					return ok
				}
			}
		} else {
			// pattern includes "/". full path match
			m = func(in string) bool {
				ok, _ := filepath.Match(e, in)
				return ok
			}
		}
		for _, in := range inputs {
			if m(in) {
				rm[in] = true
				if stepDef.rule.Debug {
					clog.Infof(ctx, "fix exclude %s by %s", in, e)
				}
			}
		}
	}
	if len(rm) == 0 {
		return inputs
	}
	r := make([]string, 0, len(inputs)-len(rm))
	for _, in := range inputs {
		if rm[in] {
			continue
		}
		r = append(r, in)
	}
	if stepDef.rule.Debug {
		clog.Infof(ctx, "fixed inputs=%d excludes=%d -> inputs=%d", len(inputs), len(excludes), len(r))
	}
	return r
}

// ExpandCaseSensitives expands inputs for the case sensitive FS.
func (s *StepDef) ExpandCaseSensitives(ctx context.Context, inputs []string) []string {
	ctx, span := trace.NewSpan(ctx, "stepdef-expand-case-sensitives")
	defer span.Close(nil)
	if len(s.globals.caseSensitives) == 0 {
		return inputs
	}
	oldLen := len(inputs)
	m := make(map[string]bool)
	var expanded []string
	for _, f := range inputs {
		f = filepath.ToSlash(f)
		if m[f] {
			continue
		}
		expanded = append(expanded, f)
		m[f] = true
		csf, ok := s.globals.caseSensitives[strings.ToLower(f)]
		if !ok {
			continue
		}
		for _, f := range csf {
			if m[f] {
				continue
			}
			expanded = append(expanded, f)
			m[f] = true
		}
	}
	newLen := len(expanded)
	if oldLen != newLen {
		clog.Infof(ctx, "expand case-sensitive %d -> %d", oldLen, newLen)
	}
	return expanded
}

// expandLabels expands labels in given inputs.
func (s *StepDef) expandLabels(ctx context.Context, inputs []string) []string {
	ctx, span := trace.NewSpan(ctx, "stepdef-expand-labels")
	defer span.Close(nil)
	if s.rule.Debug {
		clog.Infof(ctx, "expands labels")
	}
	var hasLabel bool
	for _, input := range inputs {
		if strings.Contains(input, ":") {
			hasLabel = true
			break
		}
	}
	if !hasLabel {
		return uniqueFiles(inputs)
	}
	p := s.globals.path
	seen := make(map[string]bool)
	var expanded []string
	for i := 0; i < len(inputs); i++ {
		path := inputs[i]
		if seen[path] {
			continue
		}
		seen[path] = true
		if !strings.Contains(path, ":") {
			expanded = append(expanded, path)
			continue
		}
		cpath := toConfigPath(p, path)
		deps, ok := s.globals.stepConfig.InputDeps[cpath]
		if !ok {
			// TODO(b/266759797): make it hard error?
			clog.Warningf(ctx, "unknown label %q", cpath)
			continue
		}
		if s.rule.Debug {
			clog.Infof(ctx, "expand %s", cpath)
		}
		for _, dep := range deps {
			dep := fromConfigPath(p, dep)
			inputs = append(inputs, dep)
		}
	}
	return expanded
}

// ExpandedInputs returns expanded inputs, i.e. may include indirect inputs.
func (s *StepDef) ExpandedInputs(ctx context.Context) []string {
	ctx, span := trace.NewSpan(ctx, "stepdef-expanded-inputs")
	defer span.Close(nil)
	if s.rule.Debug {
		clog.Infof(ctx, "expanded inputs")
	}
	// it takes too much memory in later build stages.
	// keep Inputs as is, and expand them when calculating digest ?
	seen := make(map[string]bool)
	var inputs []string
	for _, in := range s.edge.Inputs() {
		p := in.Path()
		p = s.globals.path.MustFromWD(p)
		if seen[p] {
			continue
		}
		seen[p] = true
		if s.rule.Debug {
			clog.Infof(ctx, "input from ninja: %s", p)
		}
		inputs = append(inputs, p)
	}
	for _, p := range edgeSolibs(s.edge) {
		p = s.globals.path.MustFromWD(p)
		if seen[p] {
			continue
		}
		if s.rule.Debug {
			clog.Infof(ctx, "input from ninja solibs: %s", p)
		}
		inputs = append(inputs, p)
	}
	for _, p := range s.rule.Inputs {
		if seen[p] {
			continue
		}
		seen[p] = true
		if s.rule.Debug {
			clog.Infof(ctx, "input from rule: %s", p)
		}
		inputs = append(inputs, p)
	}
	if s.rule.IndirectInputs.enabled() {
		if s.rule.Debug {
			clog.Infof(ctx, "indirect inputs")
		}
		filter := s.rule.IndirectInputs.filter(ctx)
		for _, in := range s.edge.Inputs() {
			edge, ok := in.InEdge()
			if !ok {
				continue
			}
			inputs = s.appendIndirectInputs(ctx, filter, edge, inputs, seen)
		}
		// and need to expand inputs for toolchain input etc.
	}

	inputs = s.globals.stepConfig.ExpandInputs(ctx, s.globals.path, s.globals.hashFS, inputs)
	var newInputs []string
	changed := false
	for i := 0; i < len(inputs); i++ {
		ins, ok := s.globals.replaces[inputs[i]]
		if ok {
			if s.rule.Debug {
				clog.Infof(ctx, "replace %s", inputs[i])
			}
			for _, in := range ins {
				if seen[in] {
					continue
				}
				seen[in] = true
				if s.rule.Debug {
					clog.Infof(ctx, "replace %s -> %s", inputs[i], in)
				}
				inputs = append(inputs, in)
			}
			changed = true
			continue
		}
		newInputs = append(newInputs, inputs[i])
		ins, ok = s.globals.accumulates[inputs[i]]
		if ok {
			if s.rule.Debug {
				clog.Infof(ctx, "accumulate %s", inputs[i])
			}
			for _, in := range ins {
				if seen[in] {
					continue
				}
				seen[in] = true
				if s.rule.Debug {
					clog.Infof(ctx, "accumulate %s -> %s", inputs[i], in)
				}
				inputs = append(inputs, in)
			}
			changed = true
		}
	}
	newInputs = fixInputs(ctx, s, newInputs, s.rule.ExcludeInputPatterns)
	if changed {
		inputs = make([]string, len(newInputs))
		copy(inputs, newInputs)
	}
	if s.rule.Debug {
		clog.Infof(ctx, "expanded inputs -> %d", len(inputs))
	}
	return inputs
}

// appendIndirectInputs appends indirect inputs edge into inputs that matches with filter function, and updates seen.
func (s *StepDef) appendIndirectInputs(ctx context.Context, filter func(context.Context, string, bool) bool, edge *ninjautil.Edge, inputs []string, seen map[string]bool) []string {
	edgeName := edge.Rule().Name()
	// allow to use outputs of the edge.
	for _, out := range edge.Outputs() {
		p := out.Path()
		p = s.globals.path.MustFromWD(p)
		if seen[p] {
			continue
		}
		seen[p] = true
		if !filter(ctx, filepath.ToSlash(p), s.rule.Debug) {
			if s.rule.Debug {
				clog.Infof(ctx, "input from ninja indirect[output] ignored: %s: %s", edgeName, p)
			}
			continue
		}
		if s.rule.Debug {
			clog.Infof(ctx, "input from ninja indirect[output] %s: %s", edgeName, p)
		}
		inputs = append(inputs, p)
	}
	var nextEdges []*ninjautil.Edge
	for _, in := range edge.Inputs() {
		p := in.Path()
		p = s.globals.path.MustFromWD(p)
		if seen[p] {
			continue
		}
		seen[p] = true
		edge, ok := in.InEdge()
		if ok {
			nextEdges = append(nextEdges, edge)
		}
		if !filter(ctx, filepath.ToSlash(p), s.rule.Debug) {
			if s.rule.Debug {
				clog.Infof(ctx, "input from ninja indirect[input] ignored: %s: %s", edgeName, p)
			}
			continue
		}
		if s.rule.Debug {
			clog.Infof(ctx, "input from ninja indirect[input] %s: %s", edgeName, p)
		}
		inputs = append(inputs, p)
	}
	for _, edge := range nextEdges {
		inputs = s.appendIndirectInputs(ctx, filter, edge, inputs, seen)
	}
	return inputs
}

// RemoteInputs returns remote input mappings.
func (s *StepDef) RemoteInputs() map[string]string {
	return s.rule.RemoteInputs
}

// Handle runs a handler for the cmd.
func (s *StepDef) Handle(ctx context.Context, cmd *execute.Cmd) error {
	handler := s.rule.Handler
	if handler == "" {
		return nil
	}
	err := s.globals.buildConfig.Handle(ctx, handler, s.globals.path, cmd, func() []string {
		// expand here if handler calls cmd.expand_inputs().
		// if handler is not set, expand later by depsExpandInput in build/builder.go
		inputs := s.ExpandedInputs(ctx)
		clog.Infof(ctx, "cmd.expandedInputs %d", len(inputs))
		return inputs
	})
	if err != nil {
		return err
	}
	// handler may use labels in inputs, so expand here.
	// TODO(ukai): always need to expand labels here?
	cmd.Inputs = s.expandLabels(ctx, cmd.Inputs)
	return nil
}

// Outputs returns outputs of the step.
func (s *StepDef) Outputs() []string {
	seen := make(map[string]bool)
	var targets []string
	for _, out := range s.edge.Outputs() {
		p := out.Path()
		p = s.globals.path.MustFromWD(p)
		if seen[p] {
			continue
		}
		seen[p] = true
		targets = append(targets, p)
	}
	targets = append(targets, s.rule.Outputs...)
	return uniqueFiles(targets)
}

// LocalOutputs returns outputs of the step that should be written to local disk.
func (s *StepDef) LocalOutputs() []string {
	if s.rule.OutputLocal {
		return s.Outputs()
	}
	return nil
}

// Pure checks if the step is pure or not.
func (s *StepDef) Pure() bool {
	return s.pure
}

// Platform returns platform properties for remote execution.
func (s *StepDef) Platform() map[string]string {
	return s.rule.Platform
}

// RecordDeps records deps of the step.
func (s *StepDef) RecordDeps(ctx context.Context, output string, t time.Time, deps []string) (bool, error) {
	return s.globals.depsLog.Record(ctx, output, t, deps)
}

// RuleFix shows suggested fix for the rule.
func (s *StepDef) RuleFix(ctx context.Context, inadds, outadds []string) []byte {
	rule := s.rule
	rule.ActionName = s.ActionName()
	var actionOut string
	if len(s.Outputs()) > 0 {
		actionOut = toConfigPath(s.globals.path, s.Outputs()[0])
	}
	rule.ActionOuts = nil
	rule.CommandPrefix = strings.Join(s.Args(ctx), " ")
	if rule.OutputsMap == nil {
		rule.OutputsMap = make(map[string]StepDeps)
	}
	rule.Inputs = nil
	deps := rule.OutputsMap[actionOut]
	for _, in := range inadds {
		in = toConfigPath(s.globals.path, in)
		deps.Inputs = append(deps.Inputs, in)
	}
	deps.Inputs = uniqueFiles(deps.Inputs)
	sort.Strings(deps.Inputs)
	for _, out := range outadds {
		out = toConfigPath(s.globals.path, out)
		deps.Outputs = append(deps.Outputs, out)
	}
	deps.Outputs = uniqueFiles(deps.Outputs)
	sort.Strings(deps.Outputs)
	rule.OutputsMap = map[string]StepDeps{
		actionOut: deps,
	}
	ruleBuf, err := json.MarshalIndent(rule, "", " ")
	if err != nil {
		ruleBuf = []byte(err.Error())
	}
	return ruleBuf
}

func uniqueFiles(files []string) []string {
	seen := make(map[string]bool)
	ret := files[:0] // reuse the same backing store.
	for _, f := range files {
		if seen[f] {
			continue
		}
		seen[f] = true
		ret = append(ret, f)
	}
	return ret
}

// REProxyConfig returns configuration options for using reproxy.
func (s *StepDef) REProxyConfig() *execute.REProxyConfig {
	return s.rule.REProxyConfig
}
