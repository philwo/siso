// Copyright 2025 The Chromium Authors
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
	"go.chromium.org/infra/build/siso/reapi/reapitest"
)

// This test simulates trusted local uploading mode
// In this test the build will:
//   - Execute action locally
//   - Upload results of local execution back to RE
//   - Use the trusted local uploads for future remote cache hits
func TestBuild_TrustedLocal(t *testing.T) {
	ctx := context.Background()

	allOutputs := []string{
		"out/siso/gen/asserts.out",
		"out/siso/obj/foo0.inputdeps.out",
		"out/siso/obj/foo1.inputdeps.out",
		"out/siso/obj/foo.o",
	}

	// Keep a global mock RE for the lifetime of the test
	fakere := &reapitest.Fake{}

	var ds dataSource
	defer func() {
		err := ds.Close(ctx)
		if err != nil {
			t.Error(err)
		}
	}()
	ds.client = reapitest.New(ctx, t, fakere)
	ds.cache = ds.client.CacheStore()

	// Setup isolated new ninja runs with global RE
	ninja := func(t *testing.T, isRemote bool) (build.Stats, error) {
		t.Helper()
		dir := t.TempDir()
		setupFiles(t, dir, t.Name(), allOutputs)
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:   ".siso_fs_state",
			OutputLocal: func(context.Context, string) bool { return true },
			DataSource:  ds,
		})
		defer cleanup()
		bcache, err := build.NewCache(ctx, build.CacheOptions{
			Store:      ds.cache,
			EnableRead: true,
		})
		if err != nil {
			return build.Stats{}, err
		}
		opt.Cache = bcache
		opt.RECacheEnableRead = true
		opt.RECacheEnableWrite = true
		opt.REAPIClient = ds.client
		opt.OutputLocal = func(context.Context, string) bool { return true }
		opt.FailuresAllowed = 0

		// Set limits to ensure local or remote action execution
		// noLimit is just a value larger than the number of test steps
		noLimit := 16
		if isRemote {
			opt.StrictRemote = true
			opt.Limits.FastLocal = 0
			opt.Limits.Remote = noLimit
		} else {
			opt.StrictRemote = false
			opt.Limits.FastLocal = noLimit
			opt.Limits.Remote = 0
		}
		stats, err := runNinja(ctx, "build.ninja", graph, opt, nil, runNinjaOpts{})

		// Make sure that outputs are present locally after build
		for _, outFile := range allOutputs {
			outPath := filepath.Join(dir, outFile)
			if _, err := os.Stat(outPath); err != nil {
				t.Errorf("output file not present: %s", outPath)
			}
		}

		return stats, err
	}

	// In the first build all action should fallback to local execution
	t.Logf("-- first build")
	stats, err := ninja(t, false)
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}
	if stats.Done != stats.Total || stats.Local != 4 || stats.CacheHit != 0 || stats.Remote != 0 {
		t.Errorf("done=%d,local=%d,cache=%d,remote=%d; want done=%d,local=%d,cache=%d,remote=%d",
			stats.Done, stats.Local, stats.CacheHit, stats.Remote, stats.Total, 4, 0, 0)

	}

	// In the second build all action should have remote cache hits available
	t.Logf("-- second build should have local result in remote cache")
	stats, err = ninja(t, true)
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}
	if stats.Done != stats.Total || stats.Local != 0 || stats.CacheHit != 4 || stats.Remote != 0 {
		t.Errorf("done=%d,local=%d,cache=%d,remote=%d; want done=%d,local=%d,cache=%d,remote=%d",
			stats.Done, stats.Local, stats.CacheHit, stats.Remote, stats.Total, 0, 4, 0)
	}

}
