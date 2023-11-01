// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjabuild

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"infra/build/siso/build"
	"infra/build/siso/build/buildconfig"
	"infra/build/siso/hashfs"
	"infra/build/siso/toolsupport/ninjautil"
)

func TestTargets(t *testing.T) {
	ctx := context.Background()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	err = os.MkdirAll(filepath.Join(dir, "build/config/siso"), 0755)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(filepath.Join(dir, "build/config/siso/main.star"), []byte(`
load("@builtin//struct.star", "module")

def init(ctx):
  return module(
    "config",
    step_config = "{}",
    filegroups = {},
    handlers = {},
  )
`), 0644)
	if err != nil {
		t.Fatal(err)
	}
	config, err := buildconfig.New(ctx, "@config//main.star", map[string]string{}, map[string]fs.FS{
		"config":           os.DirFS(filepath.Join(dir, "build/config/siso")),
		"config_overrides": os.DirFS(filepath.Join(dir, ".siso_remote")),
	})
	if err != nil {
		t.Fatal(err)
	}
	err = os.MkdirAll(filepath.Join(dir, "out/siso"), 0755)
	if err != nil {
		t.Fatal(err)
	}
	err = os.Chdir(filepath.Join(dir, "out/siso"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		err = os.Chdir(wd)
		if err != nil {
			t.Fatal(err)
		}
	}()
	path := build.NewPath(dir, "out/siso")
	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(filepath.Join(dir, "out/siso/build.ninja"), []byte(`
rule cxx
  command =  ../../third_party/llvm-build/Release+Asserts/bin/clang++ -MMD -MF ${out}.d ${defines} ${include_dirs} ${cflags} ${cflags_cc} -c ${in} -o ${out}
  description = CXX ${out}
  depfile = ${out}.d
  deps = gcc

rule link
  command = "python3" "../../build/toolchain/gcc_link_wrapper.py" --output="${output_dir}/${target_output_name}${output_extension}" -- ../../third_party/llvm-build/Release+Asserts/bin/clang++ ${ldflags} -o "${output_dir}/${target_output_name}${output_extension}" -Wl,--start-group @"${output_dir}/${target_output_name}${output_extension}.rsp" ${solibs} -Wl,--end-group  ${libs} ${rlibs}
  description = LINK ${output_dir}/${target_output_name}${output_extension}
  rspfile = ${output_dir}/${target_output_name}${output_extension}.rsp
  rspfile_content = ${in}

build foo.o: cxx foo.cc
build bar.o: cxx bar.cc
build exe: link foo.o bar.o
build all: phony exe
`), 0644)
	if err != nil {
		t.Fatal(err)
	}
	depsLog, err := ninjautil.NewDepsLog(ctx, ".siso_deps")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		err := depsLog.Close()
		if err != nil {
			t.Error(err)
		}
	}()
	stepConfig, err := NewStepConfig(ctx, config, path, hashFS, "build.ninja")
	if err != nil {
		t.Fatal(err)
	}

	nstate, err := Load(ctx, "build.ninja", path)
	if err != nil {
		t.Fatal(err)
	}

	g := NewGraph(ctx, "build.ninja", nstate, config, path, hashFS, stepConfig, depsLog)

	for _, tc := range []struct {
		args []string
		want []string
	}{
		{
			args: nil,
			want: []string{"out/siso/all"},
		},
		{
			args: []string{"all"},
			want: []string{"out/siso/all"},
		},
		{
			args: []string{"foo.o"},
			want: []string{"out/siso/foo.o"},
		},
		{
			args: []string{"foo.cc^"},
			want: []string{"out/siso/foo.o"},
		},
	} {
		targets, err := g.Targets(ctx, tc.args...)
		if err != nil {
			t.Errorf("g.Targets(ctx, %q)=%v, %v; want nil err", tc.args, targets, err)
			continue
		}
		var got []string
		for _, target := range targets {
			p, err := g.TargetPath(target)
			if err != nil {
				t.Errorf("g.TargetPath(%q)=%v, %v; want nil err", target, p, err)
			}
			got = append(got, p)
		}
		if diff := cmp.Diff(tc.want, got); diff != "" {
			t.Errorf("g.Targets(ctx, %q) diff -want +got:\n%s", tc.args, diff)
		}
	}

}
