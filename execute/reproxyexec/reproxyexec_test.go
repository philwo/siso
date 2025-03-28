// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package reproxyexec

import (
	"context"
	"infra/build/siso/hashfs"
	"net"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"infra/build/siso/execute"
	ppb "infra/third_party/reclient/api/proxy"
)

type fakeReproxy struct {
	ppb.UnimplementedCommandsServer

	runCommand func(context.Context, *ppb.RunRequest) (*ppb.RunResponse, error)
}

func (f fakeReproxy) RunCommand(ctx context.Context, req *ppb.RunRequest) (*ppb.RunResponse, error) {
	if f.runCommand == nil {
		return nil, status.Error(codes.Unimplemented, "")
	}
	return f.runCommand(ctx, req)
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
	ppb.RegisterCommandsServer(serv, fake)
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

func TestRun_Unauthenticated(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	defer hashFS.Close(ctx)

	setupDir := func(dname string) {
		t.Helper()
		fullname := filepath.Join(dir, dname)
		err := os.MkdirAll(fullname, 0755)
		if err != nil {
			t.Fatal(err)
		}
	}
	setupFile := func(fname string) {
		t.Helper()
		setupDir(filepath.Dir(fname))
		fullname := filepath.Join(dir, fname)
		err = os.WriteFile(fullname, nil, 0644)
		if err != nil {
			t.Fatal(err)
		}
	}
	setupFile("base/base.cc")
	setupFile("third_party/llvm-build/Release+Asserts/bin/clang++")
	setupDir("out/siso/obj/base")

	addr, cleanup := setupFakeReproxy(ctx, t, fakeReproxy{
		runCommand: func(ctx context.Context, req *ppb.RunRequest) (*ppb.RunResponse, error) {
			return &ppb.RunResponse{}, status.Error(codes.Unauthenticated, "Unable to authenticate with RBE")
		},
	})
	defer cleanup()
	re := REProxyExec{
		connAddress: addr,
	}

	err = re.Run(ctx, &execute.Cmd{
		ID:       "stepid",
		ExecRoot: dir,
		Dir:      "out/siso",
		HashFS:   hashFS,
		Args:     []string{"../../third_party/llvm-build/Release+Asserts/bin/clang++", "-c", "../../base/base.cc", "-o", "obj/base/base.o"},
		Inputs:   []string{"base/base.cc", "third_party/llvm-build/Release+Asserts/bin/clang++"},
		Outputs:  []string{"out/siso/obj/base/base.o"},
		REProxyConfig: &execute.REProxyConfig{
			ExecStrategy: "remote_local_fallback",
			Labels: map[string]string{
				"type":     "compile",
				"compiler": "clang",
				"lang":     "cpp",
			},
			Platform: map[string]string{
				"container-image": "gcr.io/chops-public-images-prod/xxx@sha256:xxx",
				"OSFamily":        "Linux",
			},
		},
	})
	if status.Code(err) != codes.Unauthenticated {
		t.Errorf("Run(ctx, cmd)=%v; want %v", err, codes.Unauthenticated)
	}
}
