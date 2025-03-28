// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"infra/build/siso/build"
	"infra/build/siso/hashfs"
)

// Test restat=1 behavior.
// It will skip following steps if mtime is not updated.
// https://ninja-build.org/manual.html#:~:text=appears%20in%20commands.-,restat,-if%20present%2C%20causes
//
// restat
//
//	if present, causes Ninja to re-stat the command’s outputs after
//	execution of the command. Each output whose modification time the
//	command did not change will be treated as though it had never
//	needed to be built. This may cause the output’s reverse
//	dependencies to be removed from the list of pending build
//	actions.
func TestBuild_Restat(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	exists := func(fname string) error {
		_, err := os.Stat(filepath.Join(dir, "out/siso", fname))
		return err
	}

	hashfsOpts := hashfs.Option{
		StateFile: ".siso_fs_state",
	}

	func() {
		t.Logf("first build")
		setupFiles(t, dir, t.Name(), nil)
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfsOpts)
		defer cleanup()

		b, err := build.New(ctx, graph, opt)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			err := b.Close()
			if err != nil {
				t.Errorf("b.Close()=%v", err)
			}
		}()
		err = b.Build(ctx, "build", "all")
		if err != nil {
			t.Fatalf(`b.Build(ctx, "build", "all")=%v; want nil err`, err)
		}
		if err := exists("foo.out"); err != nil {
			t.Errorf("foo.out doesn't exist: %v", err)
		}
		if err := exists("bar.out"); err != nil {
			t.Errorf("bar.out doesn't exist: %v", err)
		}
	}()

	touch := func(fname string) {
		t.Helper()
		fullname := filepath.Join(dir, fname)
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
			if fi.ModTime().Equal(nfi.ModTime()) {
				time.Sleep(1 * time.Millisecond)
				continue
			}
			return
		}
	}

	func() {
		t.Logf("second build. touch base/foo.in, expect only foo.out is built")
		touch("base/foo.in")
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfsOpts)
		defer cleanup()
		var metricsBuffer bytes.Buffer
		opt.MetricsJSONWriter = &metricsBuffer

		b, err := build.New(ctx, graph, opt)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			err := b.Close()
			if err != nil {
				t.Errorf("b.Close()=%v", err)
			}
		}()

		err = b.Build(ctx, "build", "all")
		if err != nil {
			t.Fatalf(`b.Build(ctx, "build", "all")=%v; want nil err`, err)
		}
		stat := b.Stats()
		if stat.Skipped != 2 { // all(phony) and bar.out
			t.Errorf("Skipped=%d; want 2", stat.Skipped)
		}
		dec := json.NewDecoder(bytes.NewReader(metricsBuffer.Bytes()))
		for dec.More() {
			var m build.StepMetric
			err := dec.Decode(&m)
			if err != nil {
				t.Errorf("decode %v", err)
			}
			if m.StepID == "" {
				continue
			}
			switch filepath.Base(m.Output) {
			case "foo.out":
				if m.Err {
					t.Errorf("%s err=%t; want false", m.Output, m.Err)
				}
			default:
				t.Errorf("unexpected output %q: %#v", m.Output, m)
			}
		}
	}()

	update := func(fname string) {
		t.Helper()
		fullname := filepath.Join(dir, fname)
		buf, err := os.ReadFile(fullname)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; ; i++ {
			fi, err := os.Stat(fullname)
			if err != nil {
				t.Fatal(err)
			}
			nbuf := fmt.Sprintf("%s%d", buf, i)
			err = os.WriteFile(fullname, []byte(nbuf), 0644)
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
			t.Logf("update %s: %s -> %s", fname, fi.ModTime(), nfi.ModTime())
			return
		}
	}
	// wait a while to make sure update would trigger build
	// on ubuntu-18.04 b/301201420
	time.Sleep(1 * time.Second)

	func() {
		t.Logf("third build, update base/foo.in")
		update("base/foo.in")
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfsOpts)
		defer cleanup()
		var metricsBuffer bytes.Buffer
		opt.MetricsJSONWriter = &metricsBuffer

		b, err := build.New(ctx, graph, opt)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			err := b.Close()
			if err != nil {
				t.Errorf("b.Close()=%v", err)
			}
		}()
		err = b.Build(ctx, "build", "all")
		if err != nil {
			t.Fatalf(`b.Build(ctx, "build", "all")=%v; want nil err`, err)
		}
		stat := b.Stats()
		if stat.Skipped != 1 { // all(phony)
			t.Errorf("Skipped=%d; want 1", stat.Skipped)
			for _, fname := range []string{
				"base/foo.in",
				"base/bar.in",
				"out/siso/foo.out",
				"out/siso/bar.out",
			} {
				fi, err := opt.HashFS.Stat(ctx, dir, fname)
				if err != nil {
					t.Logf("%s: err=%v", fname, err)
				} else {
					t.Logf("%s: %s", fname, fi.ModTime())
				}
			}
		}
		dec := json.NewDecoder(bytes.NewReader(metricsBuffer.Bytes()))
		for dec.More() {
			var m build.StepMetric
			err := dec.Decode(&m)
			if err != nil {
				t.Errorf("decode %v", err)
			}
			if m.StepID == "" {
				continue
			}
			switch filepath.Base(m.Output) {
			case "foo.out", "bar.out":
				if m.Err {
					t.Errorf("%s err=%t; want false", m.Output, m.Err)
				}
			default:
				t.Errorf("unexpected output %q", m.Output)
			}
		}

	}()
}

// Test restat=1 behavior for multiple output.
// some output may keep mtime, but some output was updated.
func TestBuild_RestatMultiout(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	exists := func(fname string) error {
		_, err := os.Stat(filepath.Join(dir, "out/siso", fname))
		return err
	}

	hashfsOpts := hashfs.Option{
		StateFile: ".siso_fs_state",
	}

	func() {
		t.Logf("first build")
		setupFiles(t, dir, t.Name(), nil)
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfsOpts)
		defer cleanup()

		b, err := build.New(ctx, graph, opt)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			err := b.Close()
			if err != nil {
				t.Errorf("b.Close()=%v", err)
			}
		}()
		err = b.Build(ctx, "build", "all")
		if err != nil {
			t.Fatalf(`b.Build(ctx, "build", "all")=%v; want nil err`, err)
		}
		if err := exists("foo.out"); err != nil {
			t.Errorf("foo.out doesn't exist: %v", err)
		}
		if err := exists("foo.out2"); err != nil {
			t.Errorf("foo.out2 doesn't exist: %v", err)
		}
		if err := exists("bar.out"); err != nil {
			t.Errorf("bar.out doesn't exist: %v", err)
		}
	}()

	st, err := hashfs.Load(ctx, filepath.Join(dir, "out/siso/.siso_fs_state"))
	if err != nil {
		t.Fatal(err)
	}
	stmap := hashfs.StateMap(st)

	touch := func(fname string) {
		t.Logf("touch %s", fname)
		t.Helper()
		fullname := filepath.Join(dir, fname)
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
			if fi.ModTime().Equal(nfi.ModTime()) {
				time.Sleep(1 * time.Millisecond)
				continue
			}
			return
		}
	}

	touch("base/foo.in")

	func() {
		t.Logf("second build. touch base/foo.in, expect only foo.out2 is updated and bar.out is updated")
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfsOpts)
		defer cleanup()
		b, err := build.New(ctx, graph, opt)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			err := b.Close()
			if err != nil {
				t.Errorf("b.Close()=%v", err)
			}
		}()

		err = b.Build(ctx, "build", "all")
		if err != nil {
			t.Fatalf(`b.Build(ctx, "build", "all")=%v; want nil err`, err)
		}
		stat := b.Stats()
		if stat.Skipped != 1 { // all(phony)
			t.Errorf("Skipped=%d; want 1", stat.Skipped)
		}
	}()

	nst, err := hashfs.Load(ctx, filepath.Join(dir, "out/siso/.siso_fs_state"))
	if err != nil {
		t.Fatal(err)
	}
	nstmap := hashfs.StateMap(nst)

	fname := filepath.Join(dir, "out/siso/foo.out")
	first := stmap[fname]
	second := nstmap[fname]
	if x, y := first.GetId().GetModTime(), second.GetId().GetModTime(); x == y {
		t.Errorf("foo.out mtime %d; want not %d", x, y)
	}
	if x, y := first.GetUpdatedTime(), second.GetUpdatedTime(); x >= y {
		t.Errorf("foo.out updated time %d; want < %d", x, y)
	}
	fname = filepath.Join(dir, "out/siso/foo.out2")
	first = stmap[fname]
	second = nstmap[fname]
	if x, y := first.GetId().GetModTime(), second.GetId().GetModTime(); x >= y {
		t.Errorf("foo.out2 mtime %d; want < %d", x, y)
	}
	if x, y := first.GetUpdatedTime(), second.GetUpdatedTime(); x >= y {
		t.Errorf("foo.out2 updated time %d; want < %d", x, y)
	}
	fname = filepath.Join(dir, "out/siso/bar.out")
	first = stmap[fname]
	second = nstmap[fname]
	if x, y := first.GetId().GetModTime(), second.GetId().GetModTime(); x >= y {
		t.Errorf("bar.out mtime %d; want < %d", x, y)
	}
	if x, y := first.GetUpdatedTime(), second.GetUpdatedTime(); x >= y {
		t.Errorf("bar.out updated time %d; want < %d", x, y)
	}
}
