// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package reproxyexec

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"infra/build/siso/execute"
	"infra/build/siso/execute/reproxyexec/reproxytest"
	"infra/build/siso/hashfs"
	ppb "infra/third_party/reclient/api/proxy"
)

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

	s := reproxytest.NewServer(ctx, t, reproxytest.Fake{
		RunCommandFunc: func(ctx context.Context, req *ppb.RunRequest) (*ppb.RunResponse, error) {
			return &ppb.RunResponse{}, status.Error(codes.Unauthenticated, "Unable to authenticate with RBE")
		},
	})
	t.Cleanup(s.Close)
	re := REProxyExec{
		connAddress: s.Addr(),
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
