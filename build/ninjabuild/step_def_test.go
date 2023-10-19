// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjabuild

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"infra/build/siso/build"
	"infra/build/siso/hashfs"
	"infra/build/siso/toolsupport/ninjautil"
)

func TestStepExpandLabels(t *testing.T) {
	ctx := context.Background()
	g := &globals{
		path: build.NewPath("/b/w", "out/Default"),
		stepConfig: &StepConfig{
			InputDeps: map[string][]string{
				"component:component": {
					"component/a:a",
					"component/b",
				},
				"component/a:a": {
					"component/a/1",
					"component/a/2",
				},
			},
		},
	}
	s := &StepDef{
		globals: g,
	}

	got := s.expandLabels(ctx, []string{
		"foo/bar",
		"component:component",
	})
	want := []string{
		"foo/bar",
		"component/b",
		"component/a/1",
		"component/a/2",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("s.expandLabels(...): diff -want +got:\n%s", diff)
	}
}

func TestExpandedInputs_solibs(t *testing.T) {
	ctx := context.Background()
	state := ninjautil.NewState()
	p := ninjautil.NewManifestParser(state)
	dir := t.TempDir()
	fname := filepath.Join(dir, "build.ninja")
	err := os.WriteFile(fname, []byte(`
rule solink
  command = ...

build ./libc++.so ./libc++.so.TOC: solink

rule link
  command = ...

build ./protoc: link | ./libc++.so.TOC
   solibs = ./libc++.so

rule __rule
   command = ...

build foo.h: __rule | ./protoc
`), 0644)
	if err != nil {
		t.Fatal(err)
	}
	err = p.Load(ctx, fname)
	if err != nil {
		t.Fatal(err)
	}

	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}

	graph := &Graph{
		nstate:  state,
		visited: make(map[*ninjautil.Edge]bool),
		globals: &globals{
			path:       build.NewPath(dir, "out/Default"),
			hashFS:     hashFS,
			stepConfig: &StepConfig{},
		},
	}
	newStepDef := func(target string) *StepDef {
		node, ok := state.LookupNode(target)
		if !ok {
			t.Fatalf("target %q not found in build.ninja", target)
		}
		edge, ok := node.InEdge()
		if !ok {
			t.Fatalf("target %q has no edge", target)
		}
		return graph.newStepDef(ctx, edge, nil)
	}

	for _, target := range []string{
		"libc++.so.TOC",
		"libc++.so",
		"protoc",
	} {
		fullPath := filepath.Join(dir, "out/Default", target)
		err := os.MkdirAll(filepath.Dir(fullPath), 0755)
		if err != nil {
			t.Fatalf("MkdirAll(%q)=%v", filepath.Dir(fullPath), err)
		}
		err = os.WriteFile(fullPath, nil, 0644)
		if err != nil {
			t.Fatalf("WriteFile(%q)=%v", fullPath, err)
		}
		if newStepDef(target) == nil {
			t.Fatalf("stepDef for %q is nil?", target)
		}
	}
	s := newStepDef("foo.h")
	got := s.Inputs(ctx)
	want := []string{"out/Default/protoc"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Inputs: diff -want +got:\n%s", diff)
	}
	got = s.ExpandedInputs(ctx)
	want = []string{
		"out/Default/protoc",
		"out/Default/libc++.so.TOC",
		"out/Default/libc++.so",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ExpandedInputs: diff -want +got:\n%s", diff)
	}
}
