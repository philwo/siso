// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package hashfs_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"golang.org/x/sync/errgroup"

	"infra/build/siso/hashfs"
	"infra/build/siso/reapi/digest"
	"infra/build/siso/reapi/merkletree"
)

func TestStamp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	execRoot := t.TempDir()
	opt := hashfs.Option{}
	hfs, err := hashfs.New(ctx, opt)
	if err != nil {
		t.Fatalf("New=%v", err)
	}
	defer func() {
		err := hfs.Close(ctx)
		if err != nil {
			t.Fatalf("hfs.Close=%v", err)
		}
	}()
	// `go test -count=1000` would easily catch the race.
	for i := 0; i < 100; i++ {
		fname := filepath.ToSlash(filepath.Join("obj/components", fmt.Sprintf("%d.stamp", i)))
		t.Run(fname, func(t *testing.T) {
			t.Parallel()
			var cmdhash []byte
			now := time.Now()
			_, err := hfs.Stat(ctx, execRoot, fname)
			if err == nil {
				t.Fatalf("Stat(%s)=_, %v; want nil error", fname, err)
			}
			t.Logf("Write(%q, %v)", fname, now)
			err = hfs.WriteFile(ctx, execRoot, fname, nil, false, now, cmdhash)
			if err != nil {
				t.Errorf("Write(%s)=%v; want nil error", fname, err)
			}
			fi, err := hfs.Stat(ctx, execRoot, fname)
			if err != nil {
				t.Fatalf("Stat(%s)=_, %v; want nil error", fname, err)
			}
			if got, want := fi.Name(), filepath.Base(fname); got != want {
				t.Errorf("fi.Name()=%q; want=%q", got, want)
			}
			if fi.Size() != 0 {
				t.Errorf("fi.Size()=%d; want=0", fi.Size())
			}
			if got, want := fi.Mode(), fs.FileMode(0644); got != want {
				t.Errorf("fi.Mode()=%v; want=%v", got, want)
			}
			if got, want := fi.ModTime(), now; !got.Equal(want) {
				t.Errorf("fi.ModTime()=%v; want=%v", got, want)
			}
			if fi.IsDir() {
				t.Errorf("fi.IsDir()=true; want=false")
			}
			fullname := filepath.ToSlash(filepath.Join(execRoot, fname))
			got, ok := fi.Sys().(merkletree.Entry)
			if !ok {
				t.Fatalf("fi.Sys()=%T, want merkletree.Entry", fi.Sys())
			}
			if got.Name != fullname {
				t.Errorf("entry.Name=%q, want=%q", got.Name, fullname)
			}
			if got.Data.Digest() != digest.Empty {
				t.Errorf("entry.Data.Digest=%v, want=%v", got.Data.Digest(), digest.Empty)
			}
		})
	}
}

func setupFiles(tb testing.TB, dir string, files map[string]string) {
	tb.Helper()
	for k, v := range files {
		fname := filepath.Join(dir, k)
		err := os.MkdirAll(filepath.Dir(fname), 0755)
		if err != nil {
			tb.Fatal(err)
		}
		err = os.WriteFile(fname, []byte(v), 0644)
		if err != nil {
			tb.Fatal(err)
		}
		tb.Logf("writefile(%q, %q)", fname, v)
	}
}

func TestReadDir(t *testing.T) {
	dir := t.TempDir()
	setupFiles(t, dir, map[string]string{
		"base/base.h":        "",
		"base/debug/debug.h": "",
		"base/version.h":     "",
	})
	ctx := context.Background()
	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatalf("hashfs.New(...)=_, %v; want nil err", err)
	}
	defer hashFS.Close(ctx)
	t.Logf("base/debug")
	dents, err := hashFS.ReadDir(ctx, dir, "base/debug")
	if err != nil {
		t.Errorf("hashfs.ReadDir(ctx, %q, %q)=%v, %v; want nil err", dir, "base/debug", dents, err)
	}
	if got, want := len(dents), 1; got != want {
		t.Errorf("len(dents)=%d; want=%d", got, want)
	} else {
		if got, want := dents[0].Name(), "debug.h"; got != want {
			t.Errorf("Name=%q; want=%q", got, want)
		}
		if got, want := dents[0].IsDir(), false; got != want {
			t.Errorf("IsDir=%t; want=%t", got, want)
		}
	}

	t.Logf("check base/base.h")
	fi, err := hashFS.Stat(ctx, dir, "base/base.h")
	if err != nil {
		t.Errorf("hashfs.Stat(ctx, %q, %q)=%v, %v; want, nil err", dir, "base/base.h", fi, err)
	}

	t.Logf("base")
	dents, err = hashFS.ReadDir(ctx, dir, "base")
	if err != nil {
		t.Errorf("hashfs.ReadDir(ctx, %q, %q)=%v, %v; want nil err", dir, "base", dents, err)
	}
	if got, want := len(dents), 3; got != want {
		t.Errorf("len(dents)=%d; want=%d", got, want)
	}
	got := make([]string, 0, len(dents))
	for _, dent := range dents {
		got = append(got, dent.Name())
		// debug is dir, others are not.
		isDir := dent.Name() == "debug"
		if got, want := dent.IsDir(), isDir; got != want {
			t.Errorf("%s IsDir=%t; want=%t", dent.Name(), got, want)
		}
	}
	sort.Strings(got)
	want := []string{"base.h", "debug", "version.h"}
	sort.Strings(want)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("names; -want +got:\n%s", diff)
	}

	t.Logf("create gen/base/*.h")
	err = os.MkdirAll(filepath.Join(dir, "out/siso/gen/base"), 0755)
	if err != nil {
		t.Errorf("mkdir %s/out/siso/gen/base: %v", dir, err)
	}
	for i := 0; i < 100; i++ {
		err = os.WriteFile(filepath.Join(dir, "out/siso/gen/base", fmt.Sprintf("buildflag-%d.h", i)), nil, 0644)
		if err != nil {
			t.Errorf("writefile %s/out/siso/gen/base/buildflag-%d.h: %v", dir, i, err)
		}
	}
	// concurrent access
	var eg errgroup.Group
	var names [2][]string
	eg.Go(func() error {
		dents, err := hashFS.ReadDir(ctx, dir, "out/siso/gen/base")
		for _, dent := range dents {
			names[0] = append(names[0], dent.Name())
		}
		sort.Strings(names[0])
		return err
	})
	eg.Go(func() error {
		dents, err := hashFS.ReadDir(ctx, dir, "out/siso/gen/base")
		for _, dent := range dents {
			names[1] = append(names[1], dent.Name())
		}
		sort.Strings(names[1])
		return err
	})
	err = eg.Wait()
	if err != nil {
		t.Errorf("hashfs.ReadDir(ctx, %q, %q) %v; want nil err", dir, "out/siso/gen/base", err)
	}
	if diff := cmp.Diff(names[0], names[1]); diff != "" {
		t.Errorf("readdir diff -first +second:\n%s", diff)
	}
}

func TestMkdir(t *testing.T) {
	dir := t.TempDir()
	setupFiles(t, dir, map[string]string{
		"out/siso/gen/v8/stamp": "",
	})

	ctx := context.Background()
	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatalf("hashfs.New(...)=_, %v; want nil err", err)
	}
	defer hashFS.Close(ctx)
	t.Logf("check out/siso/gen/v8/include")
	_, err = hashFS.Stat(ctx, dir, "out/siso/gen/v8/include")
	if err == nil {
		t.Fatalf("hashfs.Stat(ctx, %q, %q)=_, nil; want err", dir, "out/siso/gen/v8/include")
	}

	err = hashFS.Mkdir(ctx, dir, "out/siso/gen/v8/include/inspector")
	if err != nil {
		t.Errorf("hashfs.Mkdir(ctx, %q, %q)=%v; want nil err", dir, "out/siso/gen/v8/include/inspector", err)
	}

	fi, err := hashFS.Stat(ctx, dir, "out/siso/gen/v8/include")
	if err != nil || !fi.IsDir() {
		t.Errorf("hashfs.Stat(ctx, %q, %q)=%v, %v; want dir, nil err", dir, "out/siso/gen/v8/include", fi, err)
	}
	mtimeInclude := fi.ModTime()
	if mtimeInclude.IsZero() {
		t.Errorf("out/siso/gen/v8/include mtime: %s", mtimeInclude)
	}
	t.Logf("out/siso/gen/v8/include mtime:%s", mtimeInclude)

	fi, err = hashFS.Stat(ctx, dir, "out/siso/gen/v8/include/inspector")
	if err != nil || !fi.IsDir() {
		t.Errorf("hashfs.Stat(ctx, %q, %q)=%v, %v; want dir, nil err", dir, "out/siso/gen/v8/include/inspector", fi, err)
	}
	mtimeInspector := fi.ModTime()
	if mtimeInspector.IsZero() {
		t.Errorf("out/siso/gen/v8/include/inspector mtime: %s", mtimeInspector)
	}
	t.Logf("out/siso/gen/v8/include/inspector mtime:%s", mtimeInspector)

	t.Logf("mkdir again. mtime preserved %s", time.Now())
	err = hashFS.Mkdir(ctx, dir, "out/siso/gen/v8/include/inspector")
	if err != nil {
		t.Errorf("hashfs.Mkdir(ctx, %q, %q)=%v; want nil err", dir, "out/siso/gen/v8/include/inspector", err)
	}

	fi, err = hashFS.Stat(ctx, dir, "out/siso/gen/v8/include")
	if err != nil || !fi.IsDir() {
		t.Errorf("hashfs.Stat(ctx, %q, %q)=%v, %v; want dir, nil err", dir, "out/siso/gen/v8/include", fi, err)
	}
	if !fi.ModTime().Equal(mtimeInclude) {
		t.Errorf("%q mtime: %s -> %s", "out/siso/gen/v8/include", mtimeInclude, fi.ModTime())
	}

	fi, err = hashFS.Stat(ctx, dir, "out/siso/gen/v8/include/inspector")
	if err != nil || !fi.IsDir() {
		t.Errorf("hashfs.Stat(ctx, %q, %q)=%v, %v; want dir, nil err", dir, "out/siso/gen/v8/include/inspector", fi, err)
	}
	if !fi.ModTime().Equal(mtimeInspector) {
		t.Errorf("%q mtime: %s -> %s", "out/siso/gen/v8/include/inspector", mtimeInspector, fi.ModTime())
	}
}

func TestStat_Race(t *testing.T) {
	dir := t.TempDir()
	fname := "third_party/breakpad/breakpad/src/google_breakpad/common/minidump_format.h"
	setupFiles(t, dir, map[string]string{
		fname: "",
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatalf("hashfs.New(...)=_, %v; want nil err", err)
	}
	defer hashFS.Close(ctx)
	var eg errgroup.Group
	// keep most cpus busy
	var count atomic.Int64
	const n = 1000
	for i := 0; i < runtime.NumCPU()-1; i++ {
		eg.Go(func() error {
			for {
				if count.Load() == n {
					break
				}
			}
			return nil
		})
	}
	for i := 0; i < n; i++ {
		eg.Go(func() error {
			defer count.Add(1)
			fi, err := hashFS.Stat(ctx, dir, fname)
			if err != nil {
				return err
			}
			if !fi.Mode().IsRegular() {
				return fmt.Errorf("mode is not regular")
			}
			return nil
		})
	}
	err = eg.Wait()
	if err != nil {
		t.Errorf("hashFS.Stat(ctx, %q, %q): %v; want nil", dir, fname, err)
	}
}

func BenchmarkStat(b *testing.B) {
	ctx := context.Background()
	dir := b.TempDir()
	opt := hashfs.Option{}
	hfs, err := hashfs.New(ctx, opt)
	if err != nil {
		b.Fatalf("New=%v", err)
	}
	defer func() {
		err := hfs.Close(ctx)
		if err != nil {
			b.Fatalf("hfs.Close=%v", err)
		}
	}()
	fname := "out/siso/gen/base/base/base.o.d"
	b.Run("not_exist", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, err := hfs.Stat(ctx, dir, fname)
			if !errors.Is(err, fs.ErrNotExist) {
				b.Fatalf("hfs.Stat(ctx,%q,%q)=%v; want %v", dir, fname, err, fs.ErrNotExist)
			}
		}
	})

	setupFiles(b, dir, map[string]string{
		fname: "",
	})
	hfs.Forget(ctx, dir, []string{fname})

	b.Run("emptyfile", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, err := hfs.Stat(ctx, dir, fname)
			if err != nil {
				b.Fatalf("hfs.Stat(ctx,%q,%q)=%v; want nil", dir, fname, err)
			}
		}
	})

}

func TestStatAllocs(t *testing.T) {
	allocBase := 0.0
	if runtime.GOOS == "windows" {
		// TODO(ukai): why it has allocations on windows only?
		allocBase = 6.0
	}

	ctx := context.Background()
	dir := t.TempDir()
	opt := hashfs.Option{}
	hfs, err := hashfs.New(ctx, opt)
	if err != nil {
		t.Fatalf("New=%v", err)
	}
	defer func() {
		err := hfs.Close(ctx)
		if err != nil {
			t.Fatalf("hfs.Close=%v", err)
		}
	}()
	fname := "out/siso/gen/base/base/base.o.d"
	t.Run("not_exist", func(t *testing.T) {
		avg := testing.AllocsPerRun(1000, func() {
			_, err := hfs.Stat(ctx, dir, fname)
			if !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("hfs.Stat(ctx,%q,%q)=%v; want %v", dir, fname, err, fs.ErrNotExist)
			}
		})
		if avg != allocBase+0 {
			t.Errorf("alloc=%f; want %f", avg, allocBase+0)
		}
	})

	setupFiles(t, dir, map[string]string{
		fname: "",
	})
	hfs.Forget(ctx, dir, []string{fname})

	t.Run("emptyfile", func(t *testing.T) {
		avg := testing.AllocsPerRun(1000, func() {
			_, err := hfs.Stat(ctx, dir, fname)
			if err != nil {
				t.Fatalf("hfs.Stat(ctx,%q,%q)=%v; want nil", dir, fname, err)
			}
		})
		if avg > allocBase+0 {
			t.Errorf("alloc=%f; want <= %f", avg, allocBase+0)
		}
	})
}

func TestUpdateFromLocal(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	opt := hashfs.Option{}
	hfs, err := hashfs.New(ctx, opt)
	if err != nil {
		t.Fatalf("New=%v", err)
	}
	defer func() {
		if hfs == nil {
			return
		}
		err := hfs.Close(ctx)
		if err != nil {
			t.Fatalf("hfs.Close=%v", err)
		}
	}()

	fname := "out/siso/gen/foo.stamp"
	fullname := filepath.Join(dir, fname)
	_, err = hfs.Stat(ctx, dir, fname)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Stat(ctx, %q,%q)=%v; want %v", dir, fname, err, fs.ErrNotExist)
	}
	setupFiles(t, dir, map[string]string{
		fname: "",
	})
	lfi, err := os.Lstat(fullname)
	if err != nil {
		t.Fatalf("lstat(%q)=%v; want nil", fullname, err)
	}
	time.Sleep(1 * time.Microsecond)
	now := time.Now()
	if now.Equal(lfi.ModTime()) {
		t.Fatalf("lfi.ModTime:%v should not be equal to now:%v", lfi.ModTime(), now)
	}
	h := sha256.New()
	h.Write([]byte("command line"))
	cmdhash := h.Sum(nil)
	err = hfs.UpdateFromLocal(ctx, dir, []string{fname}, now, cmdhash)
	if err != nil {
		t.Errorf("UpdateFromLocal(ctx, %q, {%q}, %v, cmdhash)=%v; want nil err", dir, fname, now, err)
	}
	fi, err := hfs.Stat(ctx, dir, fname)
	if err != nil {
		t.Fatalf("Stat(ctx, %q, %q)=_, %v; want nil err", dir, fname, err)
	}
	if !now.Equal(fi.ModTime()) {
		t.Errorf("fi.ModTime:%v should equal to now:%v", fi.ModTime(), now)
	}
	m := hashfs.StateMap(hfs.State(ctx))
	hfs = nil
	e, ok := m[fullname]
	if !ok {
		var keys []string
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		t.Fatalf("entry for %s not found: %q", fullname, keys)
	}
	if e.Id.ModTime != now.UnixNano() {
		t.Errorf("entry modtime=%d want=%d", e.Id.ModTime, now.UnixNano())
	}
	if !bytes.Equal(e.CmdHash, cmdhash) {
		t.Errorf("entry cmdhash=%q want=%q", hex.EncodeToString(e.CmdHash), hex.EncodeToString(cmdhash))
	}
	lfi, err = os.Lstat(fullname)
	if err != nil {
		t.Fatalf("lstat(%q)=%v; want nil", fullname, err)
	}
	if e.Id.ModTime != lfi.ModTime().UnixNano() {
		t.Errorf("entry modtime=%d lfi=%d", e.Id.ModTime, lfi.ModTime().UnixNano())
	}
}
