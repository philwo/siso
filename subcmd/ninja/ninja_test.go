// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"context"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"infra/build/siso/build"
	"infra/build/siso/build/buildconfig"
	"infra/build/siso/build/ninjabuild"
	"infra/build/siso/hashfs"
	"infra/build/siso/toolsupport/ninjautil"
	pb "infra/third_party/reclient/api/proxy"
)

func setupFiles(t *testing.T, dir, name string, deletes []string) {
	t.Helper()
	root := filepath.Join("testdata", name)
	err := filepath.Walk(root, func(pathname string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}
		name, err := filepath.Rel(root, pathname)
		if err != nil {
			return err
		}
		if info.IsDir() {
			return os.MkdirAll(filepath.Join(dir, name), 0755)
		}
		buf, err := os.ReadFile(pathname)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, name), buf, info.Mode())
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range deletes {
		err = os.Remove(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
	}
}

func setupBuild(ctx context.Context, t *testing.T, dir string, fsopt hashfs.Option) (build.Options, build.Graph, func()) {
	t.Helper()
	var cleanups []func()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cleanups = append(cleanups, func() {
		err := os.Chdir(wd)
		if err != nil {
			t.Fatal(err)
		}
	})
	err = os.MkdirAll(filepath.Join(dir, "out/siso"), 0755)
	if err != nil {
		t.Fatal(err)
	}
	err = os.Chdir(filepath.Join(dir, "out/siso"))
	if err != nil {
		t.Fatal(err)
	}
	hashFS, err := hashfs.New(ctx, fsopt)
	if err != nil {
		t.Fatal(err)
	}
	cleanups = append(cleanups, func() {
		err := hashFS.Close(ctx)
		if err != nil {
			t.Fatal(err)
		}
	})
	config, err := buildconfig.New(ctx, "@config//main.star", map[string]string{}, map[string]fs.FS{
		"config":           os.DirFS(filepath.Join(dir, "build/config/siso")),
		"config_overrides": os.DirFS(filepath.Join(dir, ".siso_remote")),
	})
	if err != nil {
		t.Fatal(err)
	}
	path := build.NewPath(dir, "out/siso")
	depsLog, err := ninjautil.NewDepsLog(ctx, ".siso_deps")
	if err != nil {
		t.Fatal(err)
	}
	cleanups = append(cleanups, func() {
		err := depsLog.Close()
		if err != nil {
			t.Fatal(err)
		}
	})
	graph, err := ninjabuild.NewGraph(ctx, "build.ninja", config, path, hashFS, depsLog)
	if err != nil {
		t.Fatal(err)
	}
	cachestore, err := build.NewLocalCache(".siso_cache")
	if err != nil {
		t.Logf("no local cache enabled: %v", err)
	}
	cache, err := build.NewCache(ctx, build.CacheOptions{
		Store: cachestore,
	})
	if err != nil {
		t.Fatal(err)
	}
	opt := build.Options{
		Path:            path,
		HashFS:          hashFS,
		Cache:           cache,
		FailuresAllowed: 1,
		Limits:          build.UnitTestLimits(ctx),
	}
	return opt, graph, func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}
}

func openDepsLog(ctx context.Context, t *testing.T, dir string) (*ninjautil.DepsLog, func()) {
	t.Helper()
	var cleanups []func()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cleanups = append(cleanups, func() {
		err := os.Chdir(wd)
		if err != nil {
			t.Fatal(err)
		}
	})
	err = os.Chdir(filepath.Join(dir, "out/siso"))
	if err != nil {
		t.Fatal(err)
	}
	depsLog, err := ninjautil.NewDepsLog(ctx, ".siso_deps")
	if err != nil {
		t.Fatal(err)
	}
	cleanups = append(cleanups, func() {
		err := depsLog.Close()
		if err != nil {
			t.Fatal(err)
		}
	})
	return depsLog, func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}
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
