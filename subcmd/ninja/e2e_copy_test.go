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
	"sort"
	"testing"

	"infra/build/siso/hashfs"
)

func TestBuild_Copy(t *testing.T) {
	ctx := context.Background()

	ninja := func(t *testing.T, dir string, outputLocal hashfs.OutputLocalFunc) error {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:   ".siso_fs_state",
			OutputLocal: outputLocal,
		})
		defer cleanup()
		_, err := runNinja(ctx, "build.ninja", graph, opt, nil, runNinjaOpts{})
		return err
	}

	// need to use this test name, not subtest name in subtests below.
	tname := t.Name()

	for _, tc := range []struct {
		name        string
		outputLocal hashfs.OutputLocalFunc
		onDisk      error
	}{
		{
			name:        "minimum",
			outputLocal: func(context.Context, string) bool { return false },
			onDisk:      fs.ErrNotExist,
		},
		{
			name:        "full",
			outputLocal: func(context.Context, string) bool { return true },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tdir := t.TempDir()
			dir, err := filepath.EvalSymlinks(tdir)
			if err != nil {
				t.Fatalf("evalsymlinks(%q)=%q, %v; want nil err", tdir, dir, err)
			}
			setupFiles(t, dir, tname, nil)
			err = ninja(t, dir, tc.outputLocal)
			if err != nil {
				t.Fatalf("ninja %v; want nil err", err)
			}
			st, err := hashfs.Load(ctx, filepath.Join(dir, "out/siso/.siso_fs_state"))
			if err != nil {
				t.Errorf("hashfs.Load=%v; want nil err", err)
			}
			m := hashfs.StateMap(st)

			wantFiles := []string{
				"out/siso/gen/cache/info.txt",
				"out/siso/gen/cache/data/data.txt",
				"out/siso/gen/cache/data/subdir/subdir.txt",
				"out/siso/gen/file",
			}
			var missing bool
			for _, fname := range wantFiles {
				fullname := filepath.Join(dir, fname)
				_, ok := m[fullname]
				if !ok {
					t.Errorf("missing %q in fs state", fname)
					missing = true
				}
				_, err := os.Stat(fullname)
				onDiskErr := tc.onDisk
				if fname == "out/siso/gen/file" {
					onDiskErr = nil // handler output are always written on the disk.
				}
				if !errors.Is(err, onDiskErr) {
					t.Errorf("%q on disk: %v; want %v", fullname, err, onDiskErr)
				}
			}
			if missing {
				var files []string
				for k := range m {
					files = append(files, k)
				}
				sort.Strings(files)
				t.Logf("fs state=%#v", files)
			}
		})
	}
}
