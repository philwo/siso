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

	"infra/build/siso/build"
	"infra/build/siso/execute/reproxyexec/reproxytest"
	"infra/build/siso/hashfs"
)

func TestBuild_Fail_Reproxy(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	ninja := func(t *testing.T, refake *reproxytest.Fake) (build.Stats, error) {
		t.Helper()
		s := reproxytest.NewServer(ctx, t, refake)
		defer s.Close()

		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile: ".siso_fs_state",
		})
		defer cleanup()
		opt.ReproxyAddr = s.Addr()
		return runNinja(ctx, "build.ninja", graph, opt, nil, runNinjaOpts{})
	}

	t.Logf("first build")
	setupFiles(t, dir, t.Name(), nil)
	fakereSuccess := &reproxytest.Fake{
		RunCommandFunc: func(ctx context.Context, req *pb.RunRequest) (*pb.RunResponse, error) {
			err := os.WriteFile(filepath.Join(dir, "out/siso/gen/foo.srcjar"), nil, 0644)
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
			}, nil
		},
	}
	stats, err := ninja(t, fakereSuccess)
	if err != nil {
		t.Fatalf("ninja %v; want nil err", err)
	}
	if stats.Done != 3 || stats.NoExec != 1 || stats.Remote != 1 || stats.Skipped != 1 {
		t.Fatalf("ninja stats done=%d NoExec=%d Remote=%d Skipped=%d; want done=3 NoExec=1 Remote=1 Skipped=1", stats.Done, stats.NoExec, stats.Remote, stats.Skipped)
	}

	t.Logf("first confirm no-op")
	stats, err = ninja(t, fakereSuccess)
	if err != nil {
		t.Fatalf("ninja %v; want nil err", err)
	}
	if stats.Done != 3 || stats.Skipped != 3 || stats.Remote != 0 || stats.Local != 0 || stats.NoExec != 0 {
		t.Fatalf("ninja confirm no-op error? stats=%#v", stats)
	}

	t.Logf("make bad foo.txt and fail gen/foo.srcjar")
	err = os.WriteFile(filepath.Join(dir, "foo.txt"), []byte("error"), 0644)
	if err != nil {
		t.Fatal(err)
	}
	fakereErr := &reproxytest.Fake{
		RunCommandFunc: func(ctx context.Context, req *pb.RunRequest) (*pb.RunResponse, error) {
			return &pb.RunResponse{
				Stderr: []byte("reproxy error"),
				Result: &cpb.CommandResult{
					Status:   cpb.CommandResultStatus_LOCAL_ERROR,
					ExitCode: 1,
				},
			}, nil
		},
	}
	stats, err = ninja(t, fakereErr)
	if err == nil {
		t.Fatalf("ninja succeeded, but want err")
	}
	if stats.Done != 1 || stats.Fail != 1 || stats.Remote != 1 {
		t.Fatalf("ninja stats done=%d Fail=%d Remote=%d; want done=1 Fail=1 Remote=1", stats.Done, stats.Fail, stats.Remote)
	}

	t.Logf("rerun ninja, should fail again")
	stats, err = ninja(t, fakereErr)
	if err == nil {
		t.Fatalf("ninja succeeded, but want err")
	}
	if stats.Done != 1 || stats.Fail != 1 || stats.Remote != 1 {
		t.Fatalf("ninja stats done=%d Fail=%d Remote=%d; want done=1 Fail=1 Remote=1", stats.Done, stats.Fail, stats.Remote)
	}

	t.Logf("fix")
	err = os.WriteFile(filepath.Join(dir, "foo.txt"), []byte("ok"), 0644)
	if err != nil {
		t.Fatal(err)
	}
	stats, err = ninja(t, fakereSuccess)
	if err != nil {
		t.Fatalf("ninja %v; want nil err", err)
	}
	if stats.Done != 3 || stats.NoExec != 1 || stats.Remote != 1 || stats.Skipped != 1 {
		t.Fatalf("ninja stats done=%d NoExec=%d Remote=%d Skipped=%d; want done=3 NoExec=1 Remote=1 Skipped=1", stats.Done, stats.NoExec, stats.Remote, stats.Skipped)
	}
}
