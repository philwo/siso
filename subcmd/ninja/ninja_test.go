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
	"time"

	"infra/build/siso/build"
	"infra/build/siso/build/buildconfig"
	"infra/build/siso/build/ninjabuild"
	"infra/build/siso/hashfs"
	"infra/build/siso/toolsupport/ninjautil"
)

func setupFiles(t *testing.T, dir, name string, deletes []string) {
	t.Helper()
	root := filepath.Join("testdata", name)
	err := filepath.Walk(root, func(pathname string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}
		name, err := filepath.Rel(root, pathname)
		if err != nil {
			return err
		}
		if info.IsDir() {
			return os.MkdirAll(filepath.Join(dir, name), 0755)
		}
		if info.Mode()&fs.ModeSymlink == fs.ModeSymlink {
			target, err := os.Readlink(pathname)
			if err != nil {
				return err
			}
			return os.Symlink(target, filepath.Join(dir, name))
		}
		buf, err := os.ReadFile(pathname)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, name), buf, info.Mode())
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range deletes {
		err = os.Remove(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
	}
}

// make sure file at dir/name is modified, i.e. have different mtime.
// gen takes old content and returns new content.
func modifyFile(t *testing.T, dir, name string, gen func([]byte) []byte) {
	t.Helper()
	t.Logf("-- modify %s", name)
	fullname := filepath.Join(dir, name)
	fi, err := os.Stat(fullname)
	if err != nil {
		t.Fatal(err)
	}
	buf, err := os.ReadFile(fullname)
	if err != nil {
		t.Fatal(err)
	}
	buf = gen(buf)
	err = os.WriteFile(fullname, buf, fi.Mode())
	if err != nil {
		t.Fatal(err)
	}
	for {
		err = os.Chtimes(fullname, time.Now(), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		nfi, err := os.Stat(fullname)
		if err != nil {
			t.Fatal(err)
		}
		if fi.ModTime().Equal(nfi.ModTime()) {
			time.Sleep(1 * time.Millisecond)
			continue
		}
		return
	}
}

// like modifyFile, make sure file at dir/name exists and mtime is updated.
func touchFile(t *testing.T, dir, name string) {
	t.Helper()
	t.Logf("-- touch %s", name)
	fullname := filepath.Join(dir, name)
	fi, err := os.Stat(fullname)
	if errors.Is(err, fs.ErrNotExist) {
		err = os.WriteFile(fullname, nil, 0644)
		if err != nil {
			t.Fatal(err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
	for {
		err = os.Chtimes(fullname, time.Now(), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		nfi, err := os.Stat(fullname)
		if err != nil {
			t.Fatal(err)
		}
		if fi != nil && fi.ModTime().Equal(nfi.ModTime()) {
			time.Sleep(1 * time.Millisecond)
			continue
		}
		return
	}
}

func setupBuild(ctx context.Context, t *testing.T, dir string, fsopt hashfs.Option) (build.Options, *ninjabuild.Graph, func()) {
	t.Helper()
	var cleanups []func()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cleanups = append(cleanups, func() {
		err := os.Chdir(wd)
		if err != nil {
			t.Fatal(err)
		}
	})
	err = os.MkdirAll(filepath.Join(dir, "out/siso"), 0755)
	if err != nil {
		t.Fatal(err)
	}
	err = os.Chdir(filepath.Join(dir, "out/siso"))
	if err != nil {
		t.Fatal(err)
	}
	hashFS, err := hashfs.New(ctx, fsopt)
	if err != nil {
		t.Fatal(err)
	}
	cleanups = append(cleanups, func() {
		err := hashFS.Close(ctx)
		if err != nil {
			t.Fatal(err)
		}
	})
	config, err := buildconfig.New(ctx, "@config//main.star", map[string]string{}, map[string]fs.FS{
		"config":           os.DirFS(filepath.Join(dir, "build/config/siso")),
		"config_overrides": os.DirFS(filepath.Join(dir, ".siso_remote")),
	})
	if err != nil {
		t.Fatal(err)
	}
	path := build.NewPath(dir, "out/siso")
	depsLog, err := ninjautil.NewDepsLog(ctx, ".siso_deps")
	if err != nil {
		t.Fatal(err)
	}
	cleanups = append(cleanups, func() {
		err := depsLog.Close()
		if err != nil {
			t.Fatal(err)
		}
	})
	stepConfig, err := ninjabuild.NewStepConfig(ctx, config, path, hashFS, "build.ninja")
	if err != nil {
		t.Fatal(err)
	}
	nstate, err := ninjabuild.Load(ctx, "build.ninja", path)
	if err != nil {
		t.Fatal(err)
	}

	graph := ninjabuild.NewGraph(ctx, "build.ninja", nstate, config, path, hashFS, stepConfig, depsLog)

	cachestore, err := build.NewLocalCache(".siso_cache")
	if err != nil {
		t.Logf("no local cache enabled: %v", err)
	}
	cache, err := build.NewCache(ctx, build.CacheOptions{
		Store: cachestore,
	})
	if err != nil {
		t.Fatal(err)
	}
	opt := build.Options{
		Path:            path,
		HashFS:          hashFS,
		Cache:           cache,
		FailuresAllowed: 1,
		Limits:          build.UnitTestLimits(ctx),
	}
	return opt, graph, func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}
}

func openDepsLog(ctx context.Context, t *testing.T, dir string) (*ninjautil.DepsLog, func()) {
	t.Helper()
	var cleanups []func()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cleanups = append(cleanups, func() {
		err := os.Chdir(wd)
		if err != nil {
			t.Fatal(err)
		}
	})
	err = os.Chdir(filepath.Join(dir, "out/siso"))
	if err != nil {
		t.Fatal(err)
	}
	depsLog, err := ninjautil.NewDepsLog(ctx, ".siso_deps")
	if err != nil {
		t.Fatal(err)
	}
	cleanups = append(cleanups, func() {
		err := depsLog.Close()
		if err != nil {
			t.Fatal(err)
		}
	})
	return depsLog, func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}
}
