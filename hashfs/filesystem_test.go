// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package hashfs_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"infra/build/siso/hashfs"
)

func TestFilesystemSub_SymlinkDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no symlink test on windows")
		return
	}
	ctx := context.Background()
	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	hfs, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatalf("New=%v", err)
	}
	defer func() {
		err := hfs.Close(ctx)
		if err != nil {
			t.Errorf("hfs.Close=%v", err)
		}
	}()

	setupDir := func(dirname string) {
		t.Helper()
		fullpath := filepath.Join(dir, dirname)
		err := os.MkdirAll(fullpath, 0755)
		if err != nil {
			t.Fatalf("os.MkdirAll(%q,0755)=%v; want nil err", fullpath, err)
		}
	}
	setupFile := func(fname string) {
		t.Helper()
		setupDir(filepath.Dir(fname))
		fullpath := filepath.Join(dir, fname)
		err := os.WriteFile(fullpath, nil, 0644)
		if err != nil {
			t.Fatalf("os.WriteFile(%q, nil, 0644)=%v; want nil err", fullpath, err)
		}
	}
	setupSymlink := func(symlink, target string) {
		t.Helper()
		setupDir(filepath.Dir(symlink))
		fullpath := filepath.Join(dir, symlink)
		err := os.Symlink(target, fullpath)
		if err != nil {
			t.Fatalf("os.Symlink(%q, %q)=%v; want nil err", target, fullpath, err)
		}
	}

	setupFile("chrome-sdk/tarballs/sysroots/usr/include/stdio.h")
	setupSymlink("chrome-sdk/symlinks/sysroots", "../tarballs/sysroots")

	fsys := hfs.FileSystem(ctx, dir)

	subdir := "chrome-sdk/symlinks/sysroots"
	sub, err := fsys.Sub(subdir)
	if err != nil {
		t.Fatalf("fsys.Sub(%q)=_, %v; want nil err", subdir, err)
	}
	fname := "usr/include/stdio.h"
	_, err = fs.Stat(sub, fname)
	if err != nil {
		t.Errorf("sub.Stat(%q)=_, %v; want nil err", fname, err)
	}
}
