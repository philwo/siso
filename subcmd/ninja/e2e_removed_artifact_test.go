// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"infra/build/siso/build"
	"infra/build/siso/hashfs"
)

func TestBuild_RemovedArtifact(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	func() {
		t.Logf("first build")
		setupFiles(t, dir, t.Name(), nil)

		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:   ".siso_fs_state",
			OutputLocal: func(context.Context, string) bool { return true },
		})
		defer cleanup()

		b, err := build.New(ctx, graph, opt)
		if err != nil {
			t.Fatal(err)
		}
		err = b.Build(ctx, "build", "all")
		if err != nil {
			t.Fatalf(`b.Build(ctx, "build", "all")=%v; want nil err`, err)
		}
		_, err = os.Stat(filepath.Join(dir, "out/siso/output1"))
		if err != nil {
			t.Errorf("build failed for output1?: %v", err)
		}
		_, err = os.Stat(filepath.Join(dir, "out/siso/output2"))
		if err != nil {
			t.Errorf("build failed for output2?: %v", err)
		}
	}()

	t.Logf("remove out/siso/output1 on the disk")
	err := os.Remove(filepath.Join(dir, "out/siso/output1"))
	if err != nil {
		t.Fatal(err)
	}

	func() {
		t.Logf("second build")
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:   ".siso_fs_state",
			OutputLocal: func(context.Context, string) bool { return true },
		})
		defer cleanup()

		b, err := build.New(ctx, graph, opt)
		if err != nil {
			t.Fatal(err)
		}
		err = b.Build(ctx, "build", "all")
		if err != nil {
			t.Fatalf(`b.Build(ctx, "build", "all")=%v; want nil err`, err)
		}
		_, err = os.Stat(filepath.Join(dir, "out/siso/output1"))
		if err != nil {
			t.Errorf("build failed for output1?: %v", err)
		}
		_, err = os.Stat(filepath.Join(dir, "out/siso/output2"))
		if err != nil {
			t.Errorf("build failed for output2?: %v", err)
		}
	}()
}
