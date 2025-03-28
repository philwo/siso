// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/google/go-cmp/cmp"

	"infra/build/siso/execute"
	"infra/build/siso/hashfs"
	"infra/build/siso/reapi/merkletree"
)

type fakeGraph struct {
	inputDeps map[string][]string
}

func (fakeGraph) Targets(ctx context.Context, args ...string) ([]string, error) {
	return args, nil
}

func (fakeGraph) StepDef(ctx context.Context, target string, next StepDef) (StepDef, []string, error) {
	return nil, nil, errors.New("not implemented")
}

func (g fakeGraph) InputDeps(ctx context.Context) map[string][]string {
	return g.inputDeps
}

func (g fakeGraph) StepLimits(ctx context.Context) map[string]int {
	return map[string]int{}
}

func TestDepsGCCFixCmdInputs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("depsGCC is not used on windows")
		return
	}
	ctx := context.Background()
	dir := t.TempDir()
	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}

	setupDir := func(dirname string) {
		t.Helper()
		fullpath := filepath.Join(dir, dirname)
		err := os.MkdirAll(fullpath, 0755)
		if err != nil {
			t.Fatal(err)
		}
	}

	setupFile := func(fname string) {
		t.Helper()
		fullpath := filepath.Join(dir, fname)
		err := os.MkdirAll(filepath.Dir(fullpath), 0755)
		if err != nil {
			t.Fatal(err)
		}
		err = os.WriteFile(fullpath, nil, 0644)
		if err != nil {
			t.Fatal(err)
		}
	}
	setupSymlink := func(fname, target string) {
		t.Helper()
		fullpath := filepath.Join(dir, fname)
		err := os.MkdirAll(filepath.Dir(fullpath), 0755)
		if err != nil {
			t.Fatal(err)
		}
		err = os.Symlink(target, fullpath)
		if err != nil {
			t.Fatal(err)
		}
	}

	setupDir("out/siso")
	setupDir("out/siso/gen")
	setupDir("out/siso/obj/base")
	setupFile("base/base.cc")
	setupFile("third_party/llvm-build/Release+Asserts/bin/clang")
	setupFile("third_party/llvm-build/Release+Asserts/bin/clang++")
	setupFile("buildtools/third_party/libc++/trunk/include/stdout.h")
	setupFile("build/mac_files/xcode_binaries/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX.sdk/somefile")
	setupSymlink("build/mac_files/xcode_binaries/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX13.3.sdk", "MacOSX.sdk")

	b := &Builder{

		path:   NewPath(dir, "out/siso"),
		hashFS: hashFS,
		graph: fakeGraph{
			inputDeps: map[string][]string{
				"build/mac_files/xcode_binaries/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX.sdk:headers": {
					"build/mac_files/xcode_binaries/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX.sdk/somefile",
				},
				"third_party/llvm-build/Release+Asserts:headers": {
					"third_party/llvm-build/Release+Asserts/bin/clang",
					"third_party/llvm-build/Release+Asserts/bin/clang++",
				},
				"buildtools/third_party/libc++/trunk/include:headers": {
					"buildtools/third_party/libc++/trunk/include/stdout.h",
				},
			},
		},
	}

	cmd := &execute.Cmd{
		Args: []string{
			"../../third_party/llvm-build/Release+Asserts/bin/clang",
			"-I.",
			"-Igen",
			"-I../..",
			"-isysroot",
			"../../build/mac_files/xcode_binaries/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX13.3.sdk",
			"-isystem",
			"../../buildtools/third_party/libc++/trunk/include",
			"-c",
			"../../base/base.cc",
			"-o",
			"obj/base/base.o",
		},
		Dir: "out/siso",
	}

	gcc := depsGCC{
		treeInput: func(ctx context.Context, dir string) (merkletree.TreeEntry, error) {
			inputDeps := b.graph.InputDeps(ctx)
			if _, ok := inputDeps[dir+":headers"]; ok {
				t.Logf("tree of %s: exists", dir)
				return merkletree.TreeEntry{Name: dir}, nil
			}
			t.Logf("tree of %s: not found", dir)
			return merkletree.TreeEntry{}, errors.New("not in input_deps")
		},
	}

	got, err := gcc.fixCmdInputs(ctx, b, cmd)
	if err != nil {
		t.Errorf("gcc.fixCmdInputs(ctx, b, cmd)=%q, %v; want nil err", got, err)
	}
	want := []string{
		".",
		"build/mac_files/xcode_binaries/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX13.3.sdk",
		"buildtools/third_party/libc++/trunk/include",
		"out/siso",
		"out/siso/gen",
		"third_party/llvm-build/Release+Asserts",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("gcc.fixCmdInputs(ctx, b, cmd): diff -want +got:\n%s", diff)
	}

	wantTrees := []merkletree.TreeEntry{
		{Name: "third_party/llvm-build/Release+Asserts"},
		{Name: "build/mac_files/xcode_binaries/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX.sdk"},
		{Name: "buildtools/third_party/libc++/trunk/include"},
	}
	if diff := cmp.Diff(wantTrees, cmd.TreeInputs); diff != "" {
		t.Errorf("cmd.TreeInputs; diff -want +got:\n%s", diff)
	}
}
