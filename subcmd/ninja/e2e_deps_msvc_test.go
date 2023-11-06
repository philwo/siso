// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	pb "github.com/bazelbuild/reclient/api/proxy"
	cpb "github.com/bazelbuild/remote-apis-sdks/go/api/command"
	"github.com/google/go-cmp/cmp"

	"infra/build/siso/build"
	"infra/build/siso/execute/reproxyexec/reproxytest"
	"infra/build/siso/hashfs"
)

func TestBuild_DepsMSVC(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	func() {
		t.Logf("first build")
		setupFiles(t, dir, t.Name(), nil)
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{})
		defer cleanup()

		b, err := build.New(ctx, graph, opt)
		if err != nil {
			t.Fatal(err)
		}
		err = b.Build(ctx, "build", "all")
		if err != nil {
			t.Fatalf(`b.Build(ctx, "build", "all")=%v; want nil err`, err)
		}
	}()

	func() {
		t.Logf("first_check_deps")
		depsLog, cleanup := openDepsLog(ctx, t, dir)
		defer cleanup()
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
	}()

	func() {
		t.Logf("second build")
		setupFiles(t, dir, t.Name()+"_second", []string{"base/other.h"})
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{})
		defer cleanup()

		b, err := build.New(ctx, graph, opt)
		if err != nil {
			t.Fatal(err)
		}
		err = b.Build(ctx, "build", "all")
		if err != nil {
			t.Fatalf(`b.Build(ctx, "build", "all")=%v; want nil err`, err)
		}
	}()

	func() {
		t.Logf("second_check_deps")
		depsLog, cleanup := openDepsLog(ctx, t, dir)
		defer cleanup()
		deps, mtime, err := depsLog.Get(ctx, "foo.o")
		if err != nil {
			t.Fatalf(`depsLog.Get(ctx, "foo.o")=%v, %v, %v; want nil err`, deps, mtime, err)
		}
		want := []string{
			"../../base/foo.h",
			"../../other/other.h",
			"../../base/foo.cc",
		}
		if diff := cmp.Diff(want, deps); diff != "" {
			t.Errorf("deps for foo.o: diff -want +got:\n%s", diff)
		}
	}()
}

func TestBuild_DepsMSVC_Reproxy(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	func() {
		t.Logf("first build")
		setupFiles(t, dir, t.Name(), nil)
		s := reproxytest.NewServer(ctx, t, &reproxytest.Fake{
			RunCommandFunc: func(ctx context.Context, req *pb.RunRequest) (*pb.RunResponse, error) {
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
		defer s.Close()

		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{})
		defer cleanup()
		opt.ReproxyAddr = s.Addr()

		b, err := build.New(ctx, graph, opt)
		if err != nil {
			t.Fatal(err)
		}
		err = b.Build(ctx, "build", "all")
		if err != nil {
			t.Fatalf(`b.Build(ctx, "build", "all")=%v; want nil err`, err)
		}
	}()

	func() {
		t.Logf("first_check_deps")
		depsLog, cleanup := openDepsLog(ctx, t, dir)
		defer cleanup()
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
	}()

	func() {
		t.Logf("second build")
		setupFiles(t, dir, t.Name()+"_second", []string{"base/other.h"})
		s := reproxytest.NewServer(ctx, t, &reproxytest.Fake{
			RunCommandFunc: func(ctx context.Context, req *pb.RunRequest) (*pb.RunResponse, error) {
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
		defer s.Close()

		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{})
		defer cleanup()
		opt.ReproxyAddr = s.Addr()

		b, err := build.New(ctx, graph, opt)
		if err != nil {
			t.Fatal(err)
		}
		err = b.Build(ctx, "build", "all")
		if err != nil {
			t.Fatalf(`b.Build(ctx, "build", "all")=%v; want nil err`, err)
		}
	}()

	func() {
		t.Logf("second_check_deps")
		depsLog, cleanup := openDepsLog(ctx, t, dir)
		defer cleanup()
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
	}()
}
