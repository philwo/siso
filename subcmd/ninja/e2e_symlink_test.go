// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"infra/build/siso/build"
	"infra/build/siso/hashfs"
)

// Test symlink won't modify mtime of symlink's target.
func TestBuild_Symlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink not available on windows")
		return
	}
	ctx := context.Background()
	dir := t.TempDir()

	ninja := func(t *testing.T) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile: ".siso_fs_state",
		})
		defer cleanup()
		return runNinja(ctx, "build.ninja", graph, opt, nil, runNinjaOpts{})
	}
	setupFiles(t, dir, t.Name(), nil)

	stats, err := ninja(t)
	if err != nil {
		t.Fatalf("ninja %v: want nil err", err)
	}
	if stats.Done != stats.Total {
		t.Errorf("stats.Done=%d Total=%d", stats.Done, stats.Total)
	}

	st, err := hashfs.Load(ctx, hashfs.Option{StateFile: filepath.Join(dir, "out/siso/.siso_fs_state")})
	if err != nil {
		t.Errorf("hashfs.Load=%v; want nil err", err)
	}
	m := hashfs.StateMap(st)
	e1, ok := m[filepath.Join(dir, "out/siso/out1")]
	if !ok {
		t.Errorf("out1 not found: %v", m)
	} else {
		mtime := time.Unix(0, e1.Id.ModTime)
		fi, err := os.Lstat(filepath.Join(dir, "out/siso/out1"))
		if err != nil {
			t.Errorf("out1 not found on disk: %v", err)
		} else if !mtime.Equal(fi.ModTime()) {
			t.Errorf("out1 modtime does not match: state=%s disk=%s", mtime, fi.ModTime())
		}
	}

	t.Logf("check confirm no-op")
	stats, err = ninja(t)
	if err != nil {
		t.Fatalf("ninja %v; want nil err", err)
	}
	if stats.Skipped != stats.Total {
		t.Errorf("stats.Skipped=%d Total=%d", stats.Skipped, stats.Total)
	}
}
