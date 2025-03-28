// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"infra/build/siso/execute"
)

type fakeStepDef struct {
	actionName string
	command    string
}

func (f fakeStepDef) String() string     { return fmt.Sprintf("%#v", f) }
func (f fakeStepDef) Next() StepDef      { return nil }
func (f fakeStepDef) RuleName() string   { return "" }
func (f fakeStepDef) ActionName() string { return f.actionName }
func (f fakeStepDef) Args(context.Context) []string {
	return strings.Split(f.command, " ")
}
func (fakeStepDef) IsPhony() bool { return false }
func (f fakeStepDef) Binding(b string) string {
	switch b {
	case "command":
		return f.command
	}
	return ""
}

func (f fakeStepDef) UnescapedBinding(b string) string {
	return f.Binding(b)
}

func (fakeStepDef) Inputs(context.Context) []string             { return nil }
func (fakeStepDef) DepInputs(context.Context) ([]string, error) { return nil, nil }
func (fakeStepDef) ToolInputs(context.Context) []string         { return nil }
func (fakeStepDef) ExpandCaseSensitives(ctx context.Context, in []string) []string {
	return in
}
func (fakeStepDef) ExpandLabels(ctx context.Context, in []string) []string {
	return in
}
func (fakeStepDef) ExpandedInputs(context.Context) []string    { return nil }
func (fakeStepDef) RemoteInputs() map[string]string            { return nil }
func (fakeStepDef) REProxyConfig() execute.REProxyConfig       { return execute.REProxyConfig{} }
func (fakeStepDef) Handle(context.Context, *execute.Cmd) error { return nil }
func (fakeStepDef) Outputs() []string                          { return nil }
func (fakeStepDef) LocalOutputs() []string                     { return nil }
func (fakeStepDef) Pure() bool                                 { return false }
func (fakeStepDef) Platform() map[string]string                { return nil }
func (fakeStepDef) RecordDeps(context.Context, string, time.Time, []string) (bool, error) {
	return false, nil
}
func (fakeStepDef) RuleFix(context.Context, []string, []string) []byte { return nil }

func TestStepSpanName(t *testing.T) {
	for _, tc := range []struct {
		name    string
		stepDef fakeStepDef
		want    string
	}{
		{
			name: "cxx",
			stepDef: fakeStepDef{
				actionName: "cxx",
			},
			want: "cxx",
		},
		{
			name: "irt_x64_cxx",
			stepDef: fakeStepDef{
				actionName: "irt_x64_cxx",
			},
			want: "irt_x64_cxx",
		},
		{
			name: "mojo_generate_type_mappings.py",
			stepDef: fakeStepDef{
				actionName: "__content_browser_attribution_reporting_store_source_result_mojom_blink__type_mappings___build_toolchain_win_win_clang_x64__rule",
				command:    "python3 ../../mojo/public/tools/bindings/generate_type_mappings.py --output gen/content/browser/attribution_reporting/store_source_result_mojom_blink__type_mappings",
			},
			want: "../../mojo/public/tools/bindings/generate_type_mappings.py",
		},
		{
			name: "win_mojo_generate_type_mappings.py",
			stepDef: fakeStepDef{
				actionName: "__content_browser_attribution_reporting_store_source_result_mojom_blink__type_mappings___build_toolchain_win_win_clang_x64__rule",
				command:    "C:/b/s/w/ir/cipd_bin_packages/cpython3/bin/python3.exe ../../mojo/public/tools/bindings/generate_type_mappings.py --output gen/content/browser/attribution_reporting/store_source_result_mojom_blink__type_mappings",
			},
			want: "../../mojo/public/tools/bindings/generate_type_mappings.py",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := stepSpanName(tc.stepDef)
			if got != tc.want {
				t.Errorf("stepSpanName(stepDef)=%q; want=%q", got, tc.want)
			}
		})
	}

}
