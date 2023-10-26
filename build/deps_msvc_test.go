// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"

	"infra/build/siso/execute"
)

func TestDescMSVCDepsAfterRun(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	b := &Builder{
		path: NewPath(dir, "out/siso"),
	}
	step := &Step{
		cmd: &execute.Cmd{
			Args: []string{"clang-cl.exe", "/showIncludes", "/TP", "../../base/foo.cc"},
			Inputs: []string{
				"out/siso/clang-cl.exe",
				"base/foo.cc",
				// These headers will be added by scandeps,
				// which may add unnecessary headers from
				// difference branch of ifdef, etc.
				"base/foo.h",
				"base/bar.h",
				"v1/foo.h",
			},
			Deps: "msvc",
		},
	}
	w := step.cmd.StdoutWriter()
	_, err := w.Write([]byte("Note: including file: ../../base/foo.h\r\n"))
	if err != nil {
		t.Fatalf("write to stdout: %v", err)
	}

	var deps depsMSVC
	got, err := deps.DepsAfterRun(ctx, b, step)
	if err != nil {
		t.Errorf("DepsAfterRun(ctx, b, step)=%q, %v; want nil err", got, err)
	}
	// v1/foo.h is workaround for b/294927170 and
	// https://github.com/llvm/llvm-project/issues/58726
	want := []string{
		"../../base/foo.h",
		"../../v1/foo.h",
		"../../base/foo.cc",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("DepsAfterRun(ctx, b, step): diff -want +got:\n%s", diff)
	}
	stdout := string(step.cmd.Stdout())
	if stdout != "" {
		t.Errorf("DepsAfterRun: stdout: %q, want empty", stdout)
	}
}
