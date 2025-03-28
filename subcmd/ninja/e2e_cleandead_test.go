// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"infra/build/siso/build"
	"infra/build/siso/hashfs"
)

func TestBuild_Cleandead(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	copy := func(t *testing.T, src, dst string) {
		t.Helper()
		buf, err := os.ReadFile(filepath.Join(dir, src))
		if err != nil {
			t.Fatal(err)
		}
		fulldst := filepath.Join(dir, dst)
		err = os.MkdirAll(filepath.Dir(fulldst), 0755)
		if err != nil {
			t.Fatal(err)
		}
		err = os.WriteFile(fulldst, buf, 0644)
		if err != nil {
			t.Fatal(err)
		}
	}

	ninja := func(t *testing.T, subtool string) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile: ".siso_fs_state",
		})
		defer cleanup()
		return runNinja(ctx, "build.ninja", graph, opt, nil, runNinjaOpts{
			cleandead: true,
			subtool:   subtool,
		})
	}

	t.Logf("setup workspace")
	setupFiles(t, dir, t.Name(), nil)
	copy(t, "out/siso/build.ninja.0", "out/siso/build.ninja")

	t.Logf("first build")
	_, err := ninja(t, "")
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}
	for _, fname := range []string{
		"out/siso/gen/cache/data",
		"out/siso/gen/foo.h",
		"out/siso/gen/bar.h",
		"out/siso/obj/foo.o",
		"out/siso/obj/bar.o",
		"out/siso/target",
	} {
		_, err := os.Stat(filepath.Join(dir, fname))
		if err != nil {
			t.Errorf("stat(%q)=%v; want nil error", fname, err)
		}
	}

	t.Logf("change build.ninja")
	copy(t, "out/siso/build.ninja.1", "out/siso/build.ninja")

	t.Logf("cleandead")
	_, err = ninja(t, "cleandead")
	if err != nil {
		t.Fatalf("cleandead err: %v", err)
	}
	for _, fname := range []string{
		"out/siso/gen/bar.h",
		"out/siso/obj/bar.o",
	} {
		_, err := os.Stat(filepath.Join(dir, fname))
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("stat(%q)=%v; want %v", fname, err, fs.ErrNotExist)
		}
	}
	for _, fname := range []string{
		"out/siso/gen/cache/data",
		"out/siso/gen/foo.h",
		"out/siso/obj/foo.o",
		"out/siso/target",
	} {
		_, err := os.Stat(filepath.Join(dir, fname))
		if err != nil {
			t.Errorf("stat(%q)=%v; want nil error", fname, err)
		}
	}

}
