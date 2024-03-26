// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"infra/build/siso/build"
	"infra/build/siso/hashfs"
)

func TestBuild_PhonyDir(t *testing.T) {
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
	t.Logf("first build")
	stats, err := ninja(t)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Done != 5 || stats.Local != 3 {
		t.Errorf("done=%d local=%d; want done=5 local=3", stats.Done, stats.Local)
	}

	t.Logf("modify res.bin")
	resContent := []byte("new res.bin")
	err = os.WriteFile(filepath.Join(dir, "res.bin"), resContent, 0644)
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("second build")
	stats, err = ninja(t)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Done != 5 || stats.Local != 3 {
		t.Errorf("done=%d local=%d; want done=5 local=3", stats.Done, stats.Local)
	}

	buf, err := os.ReadFile(filepath.Join(dir, "out/siso/Foo.app/Contents/Frameworks/Foo Framework.framework/Versions/C/Resources/res.bin"))
	if err != nil {
		t.Error(err)
	}
	if !bytes.Equal(buf, resContent) {
		t.Errorf("unexpected content=%q; want=%q", buf, resContent)
	}

	t.Logf("third build. should be no-op")
	stats, err = ninja(t)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Done != 5 || stats.Local != 0 {
		t.Errorf("done=%d local=%d; want done=5 local=0", stats.Done, stats.Local)
	}
}

func TestBuild_PhonyDirCopyHandler(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	ninja := func(t *testing.T) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:   ".siso_fs_state",
			OutputLocal: func(context.Context, string) bool { return true },
		})
		defer cleanup()
		return runNinja(ctx, "build.ninja", graph, opt, nil, runNinjaOpts{})
	}

	setupFiles(t, dir, t.Name(), nil)
	t.Logf("first build")
	stats, err := ninja(t)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Done != 5 || stats.Local != 2 || stats.NoExec != 1 {
		t.Errorf("done=%d local=%d no_exec=%d; want done=5 local=2 no_exec=1", stats.Done, stats.Local, stats.NoExec)
	}

	t.Logf("modify res.bin")
	resContent := []byte("new res.bin")
	err = os.WriteFile(filepath.Join(dir, "res.bin"), resContent, 0644)
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("second build")
	stats, err = ninja(t)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Done != 5 || stats.Local != 2 || stats.NoExec != 1 {
		t.Errorf("done=%d local=%d no_exec=%d; want done=5 local=2 no_exec=1", stats.Done, stats.Local, stats.NoExec)
	}

	buf, err := os.ReadFile(filepath.Join(dir, "out/siso/Foo.app/Contents/Frameworks/Foo Framework.framework/Versions/C/Resources/res.bin"))
	if err != nil {
		t.Error(err)
	}
	if !bytes.Equal(buf, resContent) {
		t.Errorf("unexpected content=%q; want=%q", buf, resContent)
	}

	t.Logf("third build. should be no-op")
	stats, err = ninja(t)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Done != 5 || stats.Local != 0 {
		t.Errorf("done=%d local=%d; want done=5 local=0", stats.Done, stats.Local)
	}
}

func TestBuild_PhonyDirStampHandler(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	ninja := func(t *testing.T) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:   ".siso_fs_state",
			OutputLocal: func(context.Context, string) bool { return true },
		})
		defer cleanup()
		return runNinja(ctx, "build.ninja", graph, opt, nil, runNinjaOpts{})
	}

	setupFiles(t, dir, t.Name(), nil)
	t.Logf("first build")
	stats, err := ninja(t)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Done != 5 || stats.Local != 2 || stats.NoExec != 1 {
		t.Errorf("done=%d local=%d no_exec=%d; want done=5 local=2 no_exec=1", stats.Done, stats.Local, stats.NoExec)
	}

	t.Logf("modify res.bin")
	resContent := []byte("new res.bin")
	err = os.WriteFile(filepath.Join(dir, "res.bin"), resContent, 0644)
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("second build")
	stats, err = ninja(t)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Done != 5 || stats.Local != 2 || stats.NoExec != 1 {
		t.Errorf("done=%d local=%d no_exec=%d; want done=5 local=2 no_exec=1", stats.Done, stats.Local, stats.NoExec)
	}

	buf, err := os.ReadFile(filepath.Join(dir, "out/siso/Foo.app/Contents/Frameworks/Foo Framework.framework/Versions/C/Resources/res.bin"))
	if err != nil {
		t.Error(err)
	}
	if !bytes.Equal(buf, resContent) {
		t.Errorf("unexpected content=%q; want=%q", buf, resContent)
	}

	t.Logf("third build. should be no-op")
	stats, err = ninja(t)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Done != 5 || stats.Local != 0 {
		t.Errorf("done=%d local=%d; want done=5 local=0", stats.Done, stats.Local)
	}
}

func TestBuild_PhonyDirStampCopyHandler(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	ninja := func(t *testing.T) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:   ".siso_fs_state",
			OutputLocal: func(context.Context, string) bool { return true },
		})
		defer cleanup()
		return runNinja(ctx, "build.ninja", graph, opt, nil, runNinjaOpts{})
	}

	setupFiles(t, dir, t.Name(), nil)
	t.Logf("first build")
	stats, err := ninja(t)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Done != 5 || stats.Local != 1 || stats.NoExec != 2 {
		t.Errorf("done=%d local=%d no_exec=%d; want done=5 local=1 no_exec=1", stats.Done, stats.Local, stats.NoExec)
	}

	t.Logf("modify res.bin")
	resContent := []byte("new res.bin")
	err = os.WriteFile(filepath.Join(dir, "res.bin"), resContent, 0644)
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("second build")
	stats, err = ninja(t)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Done != 5 || stats.Local != 1 || stats.NoExec != 2 {
		t.Errorf("done=%d local=%d no_exec=%d; want done=5 local=1 no_exec=2", stats.Done, stats.Local, stats.NoExec)
	}

	buf, err := os.ReadFile(filepath.Join(dir, "out/siso/Foo.app/Contents/Frameworks/Foo Framework.framework/Versions/C/Resources/res.bin"))
	if err != nil {
		t.Error(err)
	}
	if !bytes.Equal(buf, resContent) {
		t.Errorf("unexpected content=%q; want=%q", buf, resContent)
	}

	t.Logf("third build. should be no-op")
	stats, err = ninja(t)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Done != 5 || stats.Local != 0 {
		t.Errorf("done=%d local=%d; want done=5 local=0", stats.Done, stats.Local)
	}
}

func TestBuild_PhonyStamp(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	ninja := func(t *testing.T) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:   ".siso_fs_state",
			OutputLocal: func(context.Context, string) bool { return true },
		})
		defer cleanup()
		return runNinja(ctx, "build.ninja", graph, opt, nil, runNinjaOpts{})
	}

	setupFiles(t, dir, t.Name(), nil)
	t.Logf("first build")
	stats, err := ninja(t)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Done != 5 || stats.Local != 3 || stats.NoExec != 0 {
		t.Errorf("done=%d local=%d no_exec=%d;  want done=5 local=3 no_exec=0", stats.Done, stats.Local, stats.NoExec)
	}
	t.Logf("modify foo/0.input")
	newContent := []byte("new 0.input")
	err = os.WriteFile(filepath.Join(dir, "foo/0.input"), newContent, 0644)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("second build")
	stats, err = ninja(t)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Done != 5 || stats.Local != 2 || stats.NoExec != 0 {
		t.Errorf("done=%d local=%d no_exec=%d;  want done=5 local=2 no_exec=0", stats.Done, stats.Local, stats.NoExec)
	}

	t.Logf("confirm no-op")
	stats, err = ninja(t)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Done != 5 || stats.Local != 0 || stats.NoExec != 0 {
		t.Errorf("done=%d local=%d no_exec=%d; want done=5 local=0 no_exec=0", stats.Done, stats.Local, stats.NoExec)
	}
}
