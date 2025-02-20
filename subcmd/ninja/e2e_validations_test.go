// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"go.chromium.org/infra/build/siso/build"
	"go.chromium.org/infra/build/siso/hashfs"
)

func TestBuild_Validations(t *testing.T) {
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
	t.Logf("-- first build")
	stats, err := ninja(t)
	if err != nil {
		t.Fatalf("ninja %v", err)
	}
	if stats.Done != stats.Total || stats.Total != 2 {
		t.Errorf("done=%d total=%d; want done=2 total=2; %#v", stats.Done, stats.Total, stats)
	}

	t.Logf("-- confirm no-op")
	stats, err = ninja(t)
	if err != nil {
		t.Fatalf("ninja %v", err)
	}
	if stats.Done != stats.Total || stats.Skipped != stats.Total || stats.Total != 2 {
		t.Errorf("done=%d total=%d skipped=%d; want done=2 total=2 skipped=2; %#v", stats.Done, stats.Total, stats.Skipped, stats)
	}
	outFI, err := os.Stat(filepath.Join(dir, "out/siso/out"))
	if err != nil {
		t.Fatal(err)
	}
	validateFI, err := os.Stat(filepath.Join(dir, "out/siso/validate"))
	if err != nil {
		t.Fatal(err)
	}

	t.Logf(`-- touch "in" only rebuilds "out"`)
	touchFile(t, dir, "in")
	stats, err = ninja(t)
	if err != nil {
		t.Fatalf("ninja %v", err)
	}
	if stats.Done != stats.Total || stats.Local != 1 || stats.Skipped != 1 || stats.Total != 2 {
		t.Errorf("done=%d total=%d local=%d skipped=%d; want done=2 total=2 local=1 skipped=1; %#v", stats.Done, stats.Total, stats.Local, stats.Skipped, stats)
	}
	outFI2, err := os.Stat(filepath.Join(dir, "out/siso/out"))
	if err != nil {
		t.Errorf("out not found? %v", err)
	}
	if !outFI2.ModTime().After(outFI.ModTime()) {
		t.Errorf("out should be updated. mtime diff=%s", outFI2.ModTime().Sub(outFI.ModTime()))
	}
	validateFI2, err := os.Stat(filepath.Join(dir, "out/siso/validate"))
	if err != nil {
		t.Errorf("validate not found? %v", err)
	}
	if !validateFI2.ModTime().Equal(validateFI.ModTime()) {
		t.Errorf("validate should not be updated. mtime diff=%s", validateFI2.ModTime().Sub(validateFI.ModTime()))
	}

	t.Logf(`-- touch "in2" only rebuilds "validate`)
	touchFile(t, dir, "in2")
	stats, err = ninja(t)
	if err != nil {
		t.Fatalf("ninja %v", err)
	}
	if stats.Done != stats.Total || stats.Local != 1 || stats.Skipped != 1 || stats.Total != 2 {
		t.Errorf("done=%d total=%d local=%d skipped=%d; want done=2 total=2 local=1 skipped=1; %#v", stats.Done, stats.Total, stats.Local, stats.Skipped, stats)
	}
	outFI3, err := os.Stat(filepath.Join(dir, "out/siso/out"))
	if err != nil {
		t.Errorf("out not found? %v", err)
	}
	if !outFI3.ModTime().Equal(outFI2.ModTime()) {
		t.Errorf("out should not be updated. mtime diff=%s", outFI3.ModTime().Sub(outFI2.ModTime()))
	}
	validateFI2, err = os.Stat(filepath.Join(dir, "out/siso/validate"))
	if err != nil {
		t.Errorf("validate not found? %v", err)
	}
	if !validateFI2.ModTime().After(validateFI.ModTime()) {
		t.Errorf("validate should be updated. mtime diff=%s", validateFI2.ModTime().Sub(validateFI.ModTime()))
	}
}

func TestBuild_ValidationsDependsOnOutput(t *testing.T) {
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
	t.Logf("-- first build")
	stats, err := ninja(t)
	if err != nil {
		t.Fatalf("ninja %v", err)
	}
	if stats.Done != stats.Total || stats.Total != 2 {
		t.Errorf("done=%d total=%d; want done=2 total=2; %#v", stats.Done, stats.Total, stats)
	}

	t.Logf("-- confirm no-op")
	stats, err = ninja(t)
	if err != nil {
		t.Fatalf("ninja %v", err)
	}
	if stats.Done != stats.Total || stats.Skipped != stats.Total || stats.Total != 2 {
		t.Errorf("done=%d total=%d skipped=%d; want done=2 total=2 skipped=2; %#v", stats.Done, stats.Total, stats.Skipped, stats)
	}
	outFI, err := os.Stat(filepath.Join(dir, "out/siso/out"))
	if err != nil {
		t.Fatal(err)
	}
	validateFI, err := os.Stat(filepath.Join(dir, "out/siso/validate"))
	if err != nil {
		t.Fatal(err)
	}

	t.Logf(`-- touch "in" rebuilds "out" and "validate"`)
	touchFile(t, dir, "in")
	stats, err = ninja(t)
	if err != nil {
		t.Fatalf("ninja %v", err)
	}
	if stats.Done != stats.Total || stats.Local != 2 || stats.Skipped != 0 || stats.Total != 2 {
		t.Errorf("done=%d total=%d local=%d skipped=%d; want done=2 total=2 local=2 skipped=0; %#v", stats.Done, stats.Total, stats.Local, stats.Skipped, stats)
	}
	outFI2, err := os.Stat(filepath.Join(dir, "out/siso/out"))
	if err != nil {
		t.Errorf("out not found? %v", err)
	}
	if !outFI2.ModTime().After(outFI.ModTime()) {
		t.Errorf("out should be updated. mtime diff=%s", outFI2.ModTime().Sub(outFI.ModTime()))
	}
	validateFI2, err := os.Stat(filepath.Join(dir, "out/siso/validate"))
	if err != nil {
		t.Errorf("validate not found? %v", err)
	}
	if !validateFI2.ModTime().After(validateFI.ModTime()) {
		t.Errorf("validate should be updated. mtime diff=%s", validateFI2.ModTime().Sub(validateFI.ModTime()))
	}

	t.Logf(`-- touch "in2" only rebuilds "validate`)
	touchFile(t, dir, "in2")
	stats, err = ninja(t)
	if err != nil {
		t.Fatalf("ninja %v", err)
	}
	if stats.Done != stats.Total || stats.Local != 1 || stats.Skipped != 1 || stats.Total != 2 {
		t.Errorf("done=%d total=%d local=%d skipped=%d; want done=2 total=2 local=1 skipped=1; %#v", stats.Done, stats.Total, stats.Local, stats.Skipped, stats)
	}
	outFI3, err := os.Stat(filepath.Join(dir, "out/siso/out"))
	if err != nil {
		t.Errorf("out not found? %v", err)
	}
	if !outFI3.ModTime().Equal(outFI2.ModTime()) {
		t.Errorf("out should not be updated. mtime diff=%s", outFI3.ModTime().Sub(outFI2.ModTime()))
	}
	validateFI3, err := os.Stat(filepath.Join(dir, "out/siso/validate"))
	if err != nil {
		t.Errorf("validate not found? %v", err)
	}
	if !validateFI3.ModTime().After(validateFI2.ModTime()) {
		t.Errorf("validate should be updated. mtime diff=%s", validateFI3.ModTime().Sub(validateFI2.ModTime()))
	}
}
