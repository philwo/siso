// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjautil

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestParser_Empty(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	err := os.WriteFile(filepath.Join(dir, "input"), nil, 0644)
	if err != nil {
		t.Fatal(err)
	}

	state := NewState()
	p := NewManifestParser(state)
	p.SetWd(dir)
	err = p.Load(ctx, "input")
	if err != nil {
		t.Errorf("Load %v", err)
	}
}

func TestParser_Rules(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	err := os.WriteFile(filepath.Join(dir, "input"), []byte(`
rule cat
  command = cat ${in} > ${out}
rule date
  command = date > $out
build result: cat in_1.cc in-2.O
`), 0644)
	if err != nil {
		t.Fatal(err)
	}

	state := NewState()
	p := NewManifestParser(state)
	p.SetWd(dir)
	err = p.Load(ctx, "input")
	if err != nil {
		t.Fatalf("Load %v", err)
	}
	node, ok := state.LookupNodeByPath("result")
	if !ok {
		t.Fatalf("missing result")
	}
	edge, ok := node.InEdge()
	if !ok {
		t.Fatalf("no inEdge for result")
	}
	if edge.RuleName() != "cat" {
		t.Errorf("RuleName=%q; want=%q", edge.RuleName(), "cat")
	}
	cmd := edge.RawBinding("command")
	want := "cat ${in} > ${out}"
	if cmd != want {
		t.Errorf("rule cat command=%q; want=%q", cmd, want)
	}
}

func TestParser_EscapedPath(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	err := os.WriteFile(filepath.Join(dir, "build.ninja"), []byte(`
rule echo
  command = echo $in > $out
build $:all: phony out
build out: echo foo$ bar $
 bar$:baz
`), 0644)
	if err != nil {
		t.Fatal(err)
	}
	state := NewState()
	p := NewManifestParser(state)
	p.SetWd(dir)
	err = p.Load(ctx, "build.ninja")
	if err != nil {
		t.Fatalf("Load %v", err)
	}
	node, ok := state.LookupNodeByPath(":all")
	if !ok {
		t.Fatalf("missing :all")
	}
	edge, ok := node.InEdge()
	if !ok {
		t.Fatalf("no inEdge for :all")
	}
	if edge.RuleName() != "phony" {
		t.Errorf("RuleName=%q; want=%q", edge.RuleName(), "phony")
	}
	ins := edge.Inputs()
	if len(ins) != 1 {
		t.Fatalf("ins=%d; want=1", len(ins))
	}
	if ins[0].Path() != "out" {
		t.Errorf("ins[0]=%q; want=%q", ins[0].Path(), "out")
	}
	edge, ok = ins[0].InEdge()
	if !ok {
		t.Fatalf("no inEdge for %q", ins[0].Path())
	}
	if edge.RuleName() != "echo" {
		t.Errorf("RuleName=%q; want=%q", edge.RuleName(), "echo")
	}
	ins = edge.Inputs()
	if len(ins) != 2 {
		t.Fatalf("ins=%d; want=2", len(ins))
	}
	if ins[0].Path() != "foo bar" {
		t.Errorf("ins[0]=%q; want=%q", ins[0].Path(), "foo bar")
	}
	if ins[1].Path() != "bar:baz" {
		t.Errorf("ins[1]=%q; want=%q", ins[1].Path(), "bar:baz")
	}
}

func TestParser_Binding_flags(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	err := os.WriteFile(filepath.Join(dir, "build.ninja"), []byte(`
ninja_required_version = 1.7.2
asmflags = -fPIC
defines = -DDCHECK_ALWAYS_ON=1
include_dirs = -I../.. -Igen

rule asm
  command = ../../third_party/llvm-build/Release+Asserts/bin/clang -MMD -MF ${out}.d ${defines} ${include_dirs} ${asmflags} -c ${in} -o ${out}
  depfile = obj/${source_name_part}.o.d
  deps = gcc
  description = ASM ${out}

build obj/armv8-linux.o: asm ../../armv8-linux.S
  source_name_part = armv8-linux
`), 0644)
	if err != nil {
		t.Fatal(err)
	}
	state := NewState()
	p := NewManifestParser(state)
	p.SetWd(dir)
	err = p.Load(ctx, "build.ninja")
	if err != nil {
		t.Fatalf("Load %v", err)
	}
	node, ok := state.LookupNodeByPath("obj/armv8-linux.o")
	if !ok {
		t.Fatalf("missing obj/armv8-linux.o")
	}
	edge, ok := node.InEdge()
	if !ok {
		t.Fatalf("no inEdge for obj/armv8-linux.o")
	}
	if edge.RuleName() != "asm" {
		t.Errorf("RuleName=%q; want=%q", edge.RuleName(), "asm")
	}
	if got, want := edge.Binding("command"), "../../third_party/llvm-build/Release+Asserts/bin/clang -MMD -MF obj/armv8-linux.o.d -DDCHECK_ALWAYS_ON=1 -I../.. -Igen -fPIC -c ../../armv8-linux.S -o obj/armv8-linux.o"; got != want {
		t.Errorf("command=%q; want=%q", got, want)
	}
	if got, want := edge.UnescapedBinding("depfile"), "obj/armv8-linux.o.d"; got != want {
		t.Errorf("depfile=%q; want=%q", got, want)
	}
	if got, want := edge.Binding("deps"), "gcc"; got != want {
		t.Errorf("deps=%q; want=%q", got, want)
	}
	if got, want := edge.Binding("description"), "ASM obj/armv8-linux.o"; got != want {
		t.Errorf("description=%q; want=%q", got, want)
	}
}

func TestParser_Binding_rsp(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	err := os.WriteFile(filepath.Join(dir, "build.ninja"), []byte(`
rule gen_buildflags
  rspfile = ${out}.rsp
  rspfile_content = -flags DCHECK_IS_CONFIGURABLE=false
  command = python3 ../../build/write_buildflag_header.py --output ${out} --rulename //base$:debugging_buildflags --gen-dir gen --definitions ${rspfile}
  restat = 1

build gen/base/debug/debugging_buildflags.h $
 : gen_buildflags $
  | $
    ../../build/write_buildflag_header.py
`), 0644)
	if err != nil {
		t.Fatal(err)
	}
	state := NewState()
	p := NewManifestParser(state)
	p.SetWd(dir)
	err = p.Load(ctx, "build.ninja")
	if err != nil {
		t.Fatalf("Load %v", err)
	}
	node, ok := state.LookupNodeByPath("gen/base/debug/debugging_buildflags.h")
	if !ok {
		t.Fatalf("missing gen/base/debug/debugging_buildflags.h")
	}
	edge, ok := node.InEdge()
	if !ok {
		t.Fatalf("no inEdge for gen/base/debug/debugging_buildflags.h")
	}
	if edge.RuleName() != "gen_buildflags" {
		t.Errorf("RuleName=%q; want=%q", edge.RuleName(), "gen_buildflags")
	}
	if got, want := edge.Binding("rspfile"), "gen/base/debug/debugging_buildflags.h.rsp"; got != want {
		t.Errorf("rspfile=%q; want=%q", got, want)
	}
	if got, want := edge.Binding("rspfile_content"), "-flags DCHECK_IS_CONFIGURABLE=false"; got != want {
		t.Errorf("rspcontent=%q; want=%q", got, want)
	}
	if got, want := edge.Binding("command"), "python3 ../../build/write_buildflag_header.py --output gen/base/debug/debugging_buildflags.h --rulename //base:debugging_buildflags --gen-dir gen --definitions gen/base/debug/debugging_buildflags.h.rsp"; got != want {
		t.Errorf("command=%q; want=%q", got, want)
	}
}

func TestParser_Binding_buildscope(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	err := os.WriteFile(filepath.Join(dir, "build.ninja"), []byte(`
pool build_toolchain_action_pool
  depth = 128

rule nocompile
  command = python3 ../../tools/nocompile/wrapper.py ../../third_party/llvm-build/Release+Asserts/bin/clang++ ${in} obj/base/${source_name_part}.o obj/base/${source_name_part}.o.d -- ${cflags} ${cflags_cc} ${defines} ${include_dirs} -MMD -MF obj/base/${source_name_part}.o.d -MT obj/base/${source_name_part}.o -x c++
  description = ACTION //base:base_nocompile_tests(//build/toolchain/linux:clang_x64)
  pool = build_toolchain_action_pool
  restat = 1

build obj/base/nocompile.o: nocompile ../../base/test/nocompile.nc
  defines =
  include_dirs =
  cflags =
  clfags_cc =
  source_name_part = nocompile
  defines = -DDCHECK_ALWAYS_ON=1
  include_dirs = -I../.. -Igen
  cflags = -Wall
  cflags_cc = -std=c++20
  depfile = obj/base/nocompile.o.d
`), 0644)
	if err != nil {
		t.Fatal(err)
	}
	state := NewState()
	p := NewManifestParser(state)
	p.SetWd(dir)
	err = p.Load(ctx, "build.ninja")
	if err != nil {
		t.Fatalf("Load %v", err)
	}
	node, ok := state.LookupNodeByPath("obj/base/nocompile.o")
	if !ok {
		t.Fatalf("missing obj/base/nocompile.o")
	}
	edge, ok := node.InEdge()
	if !ok {
		t.Fatalf("no inEdge for obj/base/nocompile.o")
	}
	if got, want := edge.Binding("command"), "python3 ../../tools/nocompile/wrapper.py ../../third_party/llvm-build/Release+Asserts/bin/clang++ ../../base/test/nocompile.nc obj/base/nocompile.o obj/base/nocompile.o.d -- -Wall -std=c++20 -DDCHECK_ALWAYS_ON=1 -I../.. -Igen -MMD -MF obj/base/nocompile.o.d -MT obj/base/nocompile.o -x c++"; got != want {
		t.Errorf("command=%q; want=%q", got, want)
	}

}

func TestParser_Dupbuild_Error(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	err := os.WriteFile(filepath.Join(dir, "build.ninja"), []byte(`
rule cat
  command = cat $in > $out
build b: cat a
build b: cat c
`), 0644)
	if err != nil {
		t.Fatal(err)
	}
	state := NewState()
	p := NewManifestParser(state)
	p.SetWd(dir)
	err = p.Load(ctx, "build.ninja")
	var wantErr multipleRulesError
	if !errors.As(err, &wantErr) {
		t.Errorf("p.Load() got: %v; want: %v", err, wantErr)
	}
}

func TestParser_ConcurrentSubninja(t *testing.T) {
	origLoaderConcurrency := loaderConcurrency
	loaderConcurrency = 8
	defer func() { loaderConcurrency = origLoaderConcurrency }()
	ctx := context.Background()
	dir := t.TempDir()

	state := NewState()
	p := NewManifestParser(state)
	p.SetWd(dir)

	write := func(fname, content string) {
		t.Helper()
		fname = filepath.Join(dir, fname)
		err := os.MkdirAll(filepath.Dir(fname), 0755)
		if err != nil {
			t.Fatal(err)
		}
		err = os.WriteFile(fname, []byte(content), 0644)
		if err != nil {
			t.Fatal(err)
		}
	}

	write("build.ninja", `
subninja a/build.ninja
subninja b/build.ninja
subninja c/build.ninja
subninja d/build.ninja
subninja e/build.ninja
subninja f/build.ninja
subninja g/build.ninja
subninja h/build.ninja
subninja i/build.ninja
`)

	for _, d := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"} {
		write(fmt.Sprintf("%s/build.ninja", d), fmt.Sprintf(`
subninja %[1]s/a/build.ninja
subninja %[1]s/a/build.ninja
subninja %[1]s/b/build.ninja
subninja %[1]s/c/build.ninja
subninja %[1]s/d/build.ninja
subninja %[1]s/e/build.ninja
subninja %[1]s/f/build.ninja
subninja %[1]s/g/build.ninja
subninja %[1]s/h/build.ninja
subninja %[1]s/i/build.ninja
`, d))
		write(fmt.Sprintf("%s/a/build.ninja", d), "")
		write(fmt.Sprintf("%s/b/build.ninja", d), "")
		write(fmt.Sprintf("%s/c/build.ninja", d), "")
		write(fmt.Sprintf("%s/d/build.ninja", d), "")
		write(fmt.Sprintf("%s/e/build.ninja", d), "")
		write(fmt.Sprintf("%s/f/build.ninja", d), "")
		write(fmt.Sprintf("%s/g/build.ninja", d), "")
		write(fmt.Sprintf("%s/h/build.ninja", d), "")
		write(fmt.Sprintf("%s/i/build.ninja", d), "")
	}

	err := p.Load(ctx, "build.ninja")
	if err != nil {
		t.Fatal(err)
	}
}

func TestParser_Validation(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	err := os.WriteFile(filepath.Join(dir, "input"), []byte(`
rule cat
   command = cat $in > $out
build foo: cat bar |@ baz baz2
`), 0644)
	if err != nil {
		t.Fatal(err)
	}
	state := NewState()
	p := NewManifestParser(state)
	p.SetWd(dir)
	err = p.Load(ctx, "input")
	if err != nil {
		t.Errorf("Load %v", err)
	}
	node, ok := state.LookupNodeByPath("foo")
	if !ok {
		t.Fatalf("foo not found")
	}
	edge, ok := node.InEdge()
	if !ok {
		t.Fatalf("no inEdge of foo")
	}
	validations := edge.Validations()
	var got []string
	for _, v := range validations {
		got = append(got, v.Path())
	}
	want := []string{"baz", "baz2"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("validations for foo: -want +got:\n%s", diff)
	}
}
