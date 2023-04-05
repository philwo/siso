// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package hashfs_test

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"infra/build/siso/hashfs"
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
