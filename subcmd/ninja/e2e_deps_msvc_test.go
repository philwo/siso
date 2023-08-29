// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"context"
	"infra/build/siso/build"
	"net"
	"os"
	"path/filepath"
	"testing"

	cpb "github.com/bazelbuild/remote-apis-sdks/go/api/command"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "infra/third_party/reclient/api/proxy"
)

func TestBuild_DepsMSVC(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	t.Run("first", func(t *testing.T) {
		setupFiles(t, dir, map[string]string{
			"build/config/siso/main.star": `
load("@builtin//struct.star", "module")
def init(ctx):
  return module(
    "config",
    step_config = "{}",
    filegroups = {},
    handlers = {},
  )
`,
			"out/siso/build.ninja": `
rule cxx
   command = python3 ../../third_party/llvm-build/Release+Asserts/bin/clang-cl.py /showIncludes:user /TP ${in} /Fo${out}
   deps = msvc

build foo.o: cxx ../../base/foo.cc
build all: phony foo.o
`,
			"base/foo.cc": `
#include "foo.h"
`,
			"base/foo.h": `
#include "other.h"
`,
			"base/other.h": "",

			"third_party/llvm-build/Release+Asserts/bin/clang-cl.py": `
print("Note: including file: ../../base/foo.h")
print("Note: including file:   ../../base/other.h")
with open("foo.o", "w") as f:
  f.write("")
`,
		})

		opt, graph := setupBuild(ctx, t, dir)
		opt.UnitTest = true

		b, err := build.New(ctx, graph, opt)
		if err != nil {
			t.Fatal(err)
		}
		err = b.Build(ctx, "build", "all")
		if err != nil {
			t.Fatalf(`b.Build(ctx, "build", "all")=%v; want nil err`, err)
		}
	})

	t.Run("first_check_deps", func(t *testing.T) {
		depsLog := openDepsLog(ctx, t, dir)
		deps, mtime, err := depsLog.Get(ctx, "foo.o")
		if err != nil {
			t.Fatalf(`depsLog.Get(ctx, "foo.o")=%v, %v, %v; want nil err`, deps, mtime, err)
		}
		want := []string{
			"../../base/foo.h",
			"../../base/other.h",
			"../../base/foo.cc",
		}
		if diff := cmp.Diff(want, deps); diff != "" {
			t.Errorf("deps for foo.o: diff -want +got:\n%s", diff)
		}
	})

	t.Run("second", func(t *testing.T) {
		setupFiles(t, dir, map[string]string{
			"base/foo.h": `
#include "other2.h"
`,
			"base/other2.h": "",

			"third_party/llvm-build/Release+Asserts/bin/clang-cl.py": `
print("Note: including file: ../../base/foo.h")
print("Note: including file:   ../../base/other2.h")
with open("foo.o", "w") as f:
  f.write("")
`,
		})
		err := os.Remove(filepath.Join(dir, "base/other.h"))
		if err != nil {
			t.Fatal(err)
		}
		opt, graph := setupBuild(ctx, t, dir)
		opt.UnitTest = true

		b, err := build.New(ctx, graph, opt)
		if err != nil {
			t.Fatal(err)
		}
		err = b.Build(ctx, "build", "all")
		if err != nil {
			t.Fatalf(`b.Build(ctx, "build", "all")=%v; want nil err`, err)
		}
	})

	t.Run("second_check_deps", func(t *testing.T) {
		depsLog := openDepsLog(ctx, t, dir)
		deps, mtime, err := depsLog.Get(ctx, "foo.o")
		if err != nil {
			t.Fatalf(`depsLog.Get(ctx, "foo.o")=%v, %v, %v; want nil err`, deps, mtime, err)
		}
		want := []string{
			"../../base/foo.h",
			"../../base/other2.h",
			"../../base/foo.cc",
		}
		if diff := cmp.Diff(want, deps); diff != "" {
			t.Errorf("deps for foo.o: diff -want +got:\n%s", diff)
		}
	})
}

type fakeReproxy struct {
	pb.UnimplementedCommandsServer

	runCommand func(context.Context, *pb.RunRequest) (*pb.RunResponse, error)
}

func (f fakeReproxy) RunCommand(ctx context.Context, req *pb.RunRequest) (*pb.RunResponse, error) {
	if f.runCommand == nil {
		return nil, status.Error(codes.Unimplemented, "")
	}
	return f.runCommand(ctx, req)
}

func (f fakeReproxy) Shutdown(ctx context.Context, req *pb.ShutdownRequest) (*pb.ShutdownResponse, error) {
	return &pb.ShutdownResponse{}, nil
}

func setupFakeReproxy(ctx context.Context, t *testing.T, fake fakeReproxy) string {
	t.Helper()

	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lis.Close() })

	addr := lis.Addr().String()
	t.Logf("fake reproxy at %s", addr)
	os.Setenv("RBE_server_address", addr)
	t.Cleanup(func() {
		os.Unsetenv("RBE_server_address")
	})

	serv := grpc.NewServer()
	pb.RegisterCommandsServer(serv, fake)
	go func() {
		err := serv.Serve(lis)
		t.Logf("Serve finished: %v", err)
	}()
	return addr
}

func TestBuild_DepsMSVC_Reproxy(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	t.Run("first", func(t *testing.T) {
		setupFiles(t, dir, map[string]string{
			"build/config/siso/main.star": `
load("@builtin//struct.star", "module")
load("@builtin//encoding.star", "json")

def __cxx(ctx, cmd):
    reproxy_config = {
      "platform": {"container-image": "gcr.io/xxx"},
      "labels": {"type": "compile", "compiler": "clang-cl", "lang": "cpp"},
      "exec_strategy": "remote",
    }
    print(json.encode(reproxy_config))
    ctx.actions.fix(reproxy_config = json.encode(reproxy_config))

__handlers = {
  "cxx": __cxx,
}

def __step_config(ctx):
   step_config = {}
   step_config["rules"] = [
     {
       "name": "cxx",
       "action": "cxx",
       "handler": "cxx",
       "debug": True,
     },
   ]
   return step_config

def init(ctx):
  return module(
    "config",
    step_config = json.encode(__step_config(ctx)),
    filegroups = {},
    handlers = __handlers,
  )
`,
			"out/siso/build.ninja": `
rule cxx
   command = ../../third_party/llvm-build/Release+Asserts/bin/clang-cl /showIncludes:user /TP ${in} /Fo${out}
   deps = msvc

build foo.o: cxx ../../base/foo.cc
build all: phony foo.o
`,
			"base/foo.cc": `
#include "foo.h"
`,
			"base/foo.h": `
#include "other.h"
`,
			"base/other.h": "",

			"third_party/llvm-build/Release+Asserts/bin/clang-cl": "",
		})

		reproxyAddr := setupFakeReproxy(ctx, t, fakeReproxy{
			runCommand: func(ctx context.Context, req *pb.RunRequest) (*pb.RunResponse, error) {
				err := os.WriteFile(filepath.Join(dir, "out/siso/foo.o"), nil, 0644)
				if err != nil {
					return &pb.RunResponse{
						Stderr: []byte(err.Error()),
						Result: &cpb.CommandResult{
							Status:   cpb.CommandResultStatus_LOCAL_ERROR,
							ExitCode: 1,
						},
					}, nil
				}
				return &pb.RunResponse{
					Result: &cpb.CommandResult{
						Status: cpb.CommandResultStatus_SUCCESS,
					},
					Stdout: []byte(`
Note: including file: ../../base/foo.h
Note: including file:   ../../base/other.h
`),
				}, nil
			},
		})

		opt, graph := setupBuild(ctx, t, dir)
		opt.UnitTest = true
		opt.ReproxyAddr = reproxyAddr

		b, err := build.New(ctx, graph, opt)
		if err != nil {
			t.Fatal(err)
		}
		err = b.Build(ctx, "build", "all")
		if err != nil {
			t.Fatalf(`b.Build(ctx, "build", "all")=%v; want nil err`, err)
		}
	})

	t.Run("first_check_deps", func(t *testing.T) {
		depsLog := openDepsLog(ctx, t, dir)
		deps, mtime, err := depsLog.Get(ctx, "foo.o")
		if err != nil {
			t.Fatalf(`depsLog.Get(ctx, "foo.o")=%v, %v, %v; want nil err`, deps, mtime, err)
		}
		want := []string{
			"../../base/foo.h",
			"../../base/other.h",
			"../../base/foo.cc",
		}
		if diff := cmp.Diff(want, deps); diff != "" {
			t.Errorf("deps for foo.o: diff -want +got:\n%s", diff)
		}
	})

	t.Run("second", func(t *testing.T) {
		setupFiles(t, dir, map[string]string{
			"base/foo.h": `
#include "other2.h"
`,
			"base/other2.h": "",

			"third_party/llvm-build/Release+Asserts/bin/clang-cl": "",
		})
		err := os.Remove(filepath.Join(dir, "base/other.h"))
		if err != nil {
			t.Fatal(err)
		}

		reproxyAddr := setupFakeReproxy(ctx, t, fakeReproxy{
			runCommand: func(ctx context.Context, req *pb.RunRequest) (*pb.RunResponse, error) {
				err := os.WriteFile(filepath.Join(dir, "out/siso/foo.o"), nil, 0644)
				if err != nil {
					return &pb.RunResponse{
						Stderr: []byte(err.Error()),
						Result: &cpb.CommandResult{
							Status:   cpb.CommandResultStatus_LOCAL_ERROR,
							ExitCode: 1,
						},
					}, nil
				}
				return &pb.RunResponse{
					Result: &cpb.CommandResult{
						Status: cpb.CommandResultStatus_SUCCESS,
					},
					Stdout: []byte(`
Note: including file: ../../base/foo.h
Note: including file:   ../../base/other2.h
`),
				}, nil
			},
		})

		opt, graph := setupBuild(ctx, t, dir)
		opt.UnitTest = true
		opt.ReproxyAddr = reproxyAddr

		b, err := build.New(ctx, graph, opt)
		if err != nil {
			t.Fatal(err)
		}
		err = b.Build(ctx, "build", "all")
		if err != nil {
			t.Fatalf(`b.Build(ctx, "build", "all")=%v; want nil err`, err)
		}
	})

	t.Run("second_check_deps", func(t *testing.T) {
		depsLog := openDepsLog(ctx, t, dir)
		deps, mtime, err := depsLog.Get(ctx, "foo.o")
		if err != nil {
			t.Fatalf(`depsLog.Get(ctx, "foo.o")=%v, %v, %v; want nil err`, deps, mtime, err)
		}
		want := []string{
			"../../base/foo.h",
			"../../base/other2.h",
			"../../base/foo.cc",
		}
		if diff := cmp.Diff(want, deps); diff != "" {
			t.Errorf("deps for foo.o: diff -want +got:\n%s", diff)
		}
	})
}
