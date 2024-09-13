// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package isolate

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"infra/build/siso/hashfs"
	"infra/build/siso/reapi/reapitest"
)

func setupFiles(t *testing.T, dir, name string) {
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
}

func setupBuildDir(ctx context.Context, t *testing.T, dir string, buildDir string) (*hashfs.HashFS, func()) {
	t.Helper()

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	err = os.MkdirAll(filepath.Join(dir, buildDir), 0755)
	if err != nil {
		t.Fatal(err)
	}
	err = os.Chdir(filepath.Join(dir, buildDir))
	if err != nil {
		t.Fatal(err)
	}
	hfs, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	return hfs, func() {
		err := os.Chdir(wd)
		if err != nil {
			t.Fatal(err)
		}
		err = hfs.Close(ctx)
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestUpload(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	setupFiles(t, dir, t.Name())
	buildDir := "out/siso"
	hfs, cleanup := setupBuildDir(ctx, t, dir, buildDir)
	defer cleanup()

	fakere := &reapitest.Fake{}
	cl := reapitest.New(ctx, t, fakere)

	target := "base_unittests"
	dg, err := upload(ctx, dir, buildDir, hfs, cl, target)
	if err != nil {
		t.Errorf("failed to upload. %v", err)
	}
	tree := reapitest.InputTree{CAS: fakere.CAS, Root: dg.Proto()}

	for _, f := range []string{
		"testing/test_runner.py",
		"testing/data/input1.txt",
		"testing/data/input2.txt",
		"testing/data/nested/input3.txt",
		buildDir + "/pyproto/proto.py",
	} {
		_, err := tree.LookupFileNode(ctx, f)
		if err != nil {
			t.Errorf("%q does not exist in the CAS tree. err=%v", f, err)
		}
	}

	// TODO: b/364131303 - Add a test case for CAS to CAS stream without output files.
}
