// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"

	"infra/build/siso/build"
	"infra/build/siso/hashfs"
	"infra/build/siso/reapi/reapitest"
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

	resContent := []byte("new res.bin")
	modifyFile(t, dir, "res.bin", func([]byte) []byte {
		return resContent
	})

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

	resContent := []byte("new res.bin")
	modifyFile(t, dir, "res.bin", func([]byte) []byte {
		return resContent
	})

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

	resContent := []byte("new res.bin")
	modifyFile(t, dir, "res.bin", func([]byte) []byte {
		return resContent
	})

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

	resContent := []byte("new res.bin")
	modifyFile(t, dir, "res.bin", func([]byte) []byte {
		return resContent
	})

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

	newContent := []byte("new 0.input")
	modifyFile(t, dir, "foo/0.input", func([]byte) []byte {
		return newContent
	})

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

func TestBuild_PhonyReplace(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	ninja := func(t *testing.T, refake *reapitest.Fake) (build.Stats, error) {
		t.Helper()
		var ds dataSource
		defer func() {
			err := ds.Close(ctx)
			if err != nil {
				t.Error(err)
			}
		}()
		ds.client = reapitest.New(ctx, t, refake)
		ds.cache = ds.client.CacheStore()

		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:  ".siso_fs_state",
			DataSource: ds,
		})
		defer cleanup()
		opt.REAPIClient = ds.client
		return runNinja(ctx, "build.ninja", graph, opt, nil, runNinjaOpts{})
	}
	setupFiles(t, dir, t.Name(), nil)

	refake := &reapitest.Fake{
		ExecuteFunc: func(fakere *reapitest.Fake, action *rpb.Action) (*rpb.ActionResult, error) {
			tree := reapitest.InputTree{CAS: fakere.CAS, Root: action.InputRootDigest}
			_, err := tree.LookupFileNode(ctx, "cp.py")
			if err != nil {
				t.Logf("missing cp.py: %v", err)
				return &rpb.ActionResult{
					ExitCode:  1,
					StderrRaw: []byte(fmt.Sprintf("../../cp.py: File not found: %v", err)),
				}, nil
			}
			fn, err := tree.LookupFileNode(ctx, "foo.in")
			if err != nil {
				t.Logf("missing foo.in: %v", err)
				return &rpb.ActionResult{
					ExitCode:  1,
					StderrRaw: []byte(fmt.Sprintf("../../foo.in: File not found: %v", err)),
				}, nil
			}
			_, err = tree.LookupFileNode(ctx, "foo2.in")
			if err != nil {
				t.Logf("missing foo2.in: %v", err)
				return &rpb.ActionResult{
					ExitCode:  1,
					StderrRaw: []byte(fmt.Sprintf("../../foo2.in: File not found: %v", err)),
				}, nil
			}
			return &rpb.ActionResult{
				ExitCode: 0,
				OutputFiles: []*rpb.OutputFile{
					{
						Path:   "foo.out",
						Digest: fn.Digest,
					},
				},
			}, nil
		},
	}

	stats, err := ninja(t, refake)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Total != 4 || stats.Done != stats.Total || stats.Remote != 1 || stats.Skipped != 3 || stats.LocalFallback != 0 {
		t.Errorf("stats total=%d done=%d remote=%d skipped=%d local_fallback=%d; want total=4 done=4 remote=1 skipped=3 local_fallback=0 (%#v)", stats.Total, stats.Done, stats.Remote, stats.Skipped, stats.LocalFallback, stats)
	}

	t.Logf("-- second build. no-op")
	stats, err = ninja(t, refake)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Total != 4 || stats.Done != stats.Total || stats.Remote != 0 || stats.Skipped != stats.Total {
		t.Errorf("stats total=%d done=%d remote=%d skipped=%d; want total=4 done=4 remote=0 skipped=4 (%#v)", stats.Total, stats.Done, stats.Remote, stats.Skipped, stats)
	}
}
