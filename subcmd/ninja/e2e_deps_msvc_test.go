// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"

	cpb "github.com/bazelbuild/remote-apis-sdks/go/api/command"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"infra/build/siso/build"
	"infra/build/siso/hashfs"
	pb "infra/third_party/reclient/api/proxy"
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
			"../../base/other2.h",
			"../../base/foo.cc",
		}
		if diff := cmp.Diff(want, deps); diff != "" {
			t.Errorf("deps for foo.o: diff -want +got:\n%s", diff)
		}
	}()
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

func setupFakeReproxy(ctx context.Context, t *testing.T, fake fakeReproxy) (string, func()) {
	t.Helper()
	var cleanups []func()

	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	cleanups = append(cleanups, func() { lis.Close() })

	addr := lis.Addr().String()
	t.Logf("fake reproxy at %s", addr)
	os.Setenv("RBE_server_address", addr)
	cleanups = append(cleanups, func() {
		os.Unsetenv("RBE_server_address")
	})

	serv := grpc.NewServer()
	pb.RegisterCommandsServer(serv, fake)
	go func() {
		err := serv.Serve(lis)
		t.Logf("Serve finished: %v", err)
	}()
	return addr, func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}
}

func TestBuild_DepsMSVC_Reproxy(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	func() {
		t.Logf("first build")
		setupFiles(t, dir, t.Name(), nil)
		reproxyAddr, recleanup := setupFakeReproxy(ctx, t, fakeReproxy{
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
		defer recleanup()

		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{})
		defer cleanup()
		opt.ReproxyAddr = reproxyAddr

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
		reproxyAddr, recleanup := setupFakeReproxy(ctx, t, fakeReproxy{
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
		defer recleanup()

		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{})
		defer cleanup()
		opt.ReproxyAddr = reproxyAddr

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
