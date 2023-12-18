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
	"sort"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/testing/protocmp"

	"infra/build/siso/hashfs"
	pb "infra/build/siso/hashfs/proto"
	"infra/build/siso/reapi/digest"
	"infra/build/siso/reapi/merkletree"
)

func TestLoadSave(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}

	fname := filepath.Join(dir, ".siso_fs_state")

	t.Logf("initial load")
	state, err := hashfs.Load(ctx, fname)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Load(...)=%v, %v; want %v", state, err, fs.ErrNotExist)
	}

	t.Logf("initial save")
	state = &pb.State{}
	err = hashfs.Save(ctx, fname, state)
	if err != nil {
		t.Errorf("Save(...)=%v; want nil", err)
	}

	t.Logf("second load")
	state, err = hashfs.Load(ctx, fname)
	if err != nil {
		t.Errorf("Load(...)=%v, %v; want nil err", state, err)
	}
	want := &pb.State{}
	if diff := cmp.Diff(want, state, protocmp.Transform()); diff != "" {
		t.Errorf("Load(...) diff -want +got:\n%s", diff)
	}

}

func TestState(t *testing.T) {
	ctx := context.Background()

	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	defer hashFS.Close(ctx)

	dir := t.TempDir()
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}

	err = hashFS.WriteFile(ctx, dir, "stamp", nil, false, time.Now(), []byte("dummy-cmdhash"))
	if err != nil {
		t.Errorf("WriteFile(...)=%v; want nil error", err)
	}

	st := hashFS.State(ctx)
	m := hashfs.StateMap(st)
	_, ok := m[filepath.Join(dir, "stamp")]
	if !ok {
		names := make([]string, 0, len(m))
		for k := range m {
			names = append(names, k)
		}
		sort.Strings(names)
		t.Errorf("stamp entry not exists? %q", names)
	}
}

func TestState_Dir(t *testing.T) {
	ctx := context.Background()

	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	defer hashFS.Close(ctx)

	dir := t.TempDir()
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}

	mtime := time.Now()
	h := sha256.New()
	fmt.Fprint(h, "step command")
	cmdhash := h.Sum(nil)
	cmdhashStr := hex.EncodeToString(cmdhash)
	d := digest.FromBytes("action digest", []byte("action proto")).Digest()
	t.Logf("record gen/generate_all dir. mtime=%s cmdhash=%s d=%s", mtime, cmdhashStr, d)
	err = update(ctx, hashFS, dir, []merkletree.Entry{
		{
			Name: "gen/generate_all",
		},
	}, mtime, cmdhash, d)
	if err != nil {
		t.Fatalf("Update %v; want nil err", err)
	}

	st := hashFS.State(ctx)
	m := hashfs.StateMap(st)
	ent, ok := m[filepath.Join(dir, "gen/generate_all")]
	if !ok {
		t.Errorf("gen/generate_all entry not exists?")
	}
	if ent.Id.ModTime != mtime.UnixNano() {
		t.Errorf("mtime=%d want=%d", ent.Id.ModTime, mtime.UnixNano())
	}
	if !bytes.Equal(ent.CmdHash, cmdhash) {
		t.Errorf("cmdhash=%s want=%s", hex.EncodeToString(ent.CmdHash), cmdhashStr)
	}
	if ent.Action.Hash != d.Hash || ent.Action.SizeBytes != d.SizeBytes {
		t.Errorf("action=%s want=%s", ent.Action, d)
	}
}

func createBenchmarkState(tb testing.TB, dir string) *pb.State {
	for i := 0; i < 60000; i++ {
		err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.txt", i)), nil, 0644)
		if err != nil {
			tb.Fatal(err)
		}
	}
	ctx := context.Background()
	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		tb.Fatal(err)
	}
	defer hashFS.Close(ctx)
	fsys := hashFS.FileSystem(ctx, dir)
	err = fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		return err
	})
	if err != nil {
		tb.Fatal(err)
	}
	st := hashFS.State(ctx)
	return st
}

func BenchmarkSetState(b *testing.B) {
	dir := b.TempDir()
	st := createBenchmarkState(b, dir)
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		hashFS, err := hashfs.New(ctx, hashfs.Option{})
		if err != nil {
			b.Fatal(err)
		}
		err = hashFS.SetState(ctx, st)
		if err != nil {
			hashFS.Close(ctx)
			b.Fatal(err)
		}
		hashFS.Close(ctx)
	}
}

func BenchmarkLoadState(b *testing.B) {
	dir := b.TempDir()
	ctx := context.Background()
	st := createBenchmarkState(b, dir)

	fname := filepath.Join(dir, ".siso_fs_state")
	err := hashfs.Save(ctx, fname, st)
	if err != nil {
		b.Fatal(err)
	}
	origState := st
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		st, err := hashfs.Load(ctx, fname)
		if err != nil {
			b.Fatal(err)
		}
		if len(st.Entries) != len(origState.Entries) {
			b.Fatalf("mismatch entries got=%d want=%d", len(st.Entries), len(origState.Entries))
		}
	}
}
