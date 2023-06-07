// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package hashfs_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"infra/build/siso/hashfs"
	"infra/build/siso/reapi/digest"
	"infra/build/siso/reapi/merkletree"
)

func TestLoadSave(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	fname := filepath.Join(dir, ".siso_fs_state")

	t.Logf("initial load")
	state, err := hashfs.Load(ctx, fname)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Load(...)=%v, %v; want %v", state, err, fs.ErrNotExist)
	}

	t.Logf("initial save")
	state = &hashfs.State{}
	err = hashfs.Save(ctx, fname, state)
	if err != nil {
		t.Errorf("Save(...)=%v; want nil", err)
	}

	t.Logf("second load")
	state, err = hashfs.Load(ctx, fname)
	if err != nil {
		t.Errorf("Load(...)=%v, %v; want nil err", state, err)
	}
	want := &hashfs.State{}
	if diff := cmp.Diff(want, state); diff != "" {
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
	err = hashFS.WriteFile(ctx, dir, "stamp", nil, false, time.Now(), []byte("dummy-cmdhash"))
	if err != nil {
		t.Errorf("WriteFile(...)=%v; want nil error", err)
	}

	st := hashFS.State(ctx)
	m := st.Map()
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

	mtime := time.Now()
	h := sha256.New()
	fmt.Fprint(h, "step command")
	cmdhash := h.Sum(nil)
	cmdhashStr := hex.EncodeToString(cmdhash)
	d := digest.FromBytes("action digest", []byte("action proto")).Digest()
	t.Logf("record gen/generate_all dir. mtime=%s cmdhash=%s d=%s", mtime, cmdhashStr, d)
	err = hashFS.Update(ctx, dir, []merkletree.Entry{
		{
			Name: "gen/generate_all",
		},
	}, mtime, cmdhash, d)
	if err != nil {
		t.Fatalf("Update %v; want nil err", err)
	}

	st := hashFS.State(ctx)
	m := st.Map()
	ent, ok := m[filepath.Join(dir, "gen/generate_all")]
	if !ok {
		t.Errorf("gen/generate_all entry not exists?")
	}
	if ent.ID.ModTime != mtime.UnixNano() {
		t.Errorf("mtime=%d want=%d", ent.ID.ModTime, mtime.UnixNano())
	}
	if ent.CmdHash != cmdhashStr {
		t.Errorf("cmdhash=%s want=%s", ent.CmdHash, cmdhashStr)
	}
	if ent.Action != d {
		t.Errorf("action=%s want=%s", ent.Action, d)
	}
}
