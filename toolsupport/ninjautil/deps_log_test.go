// Copyright 2023 The Chromium Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjautil

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// TODO(b/267409605): At minimum have test parity with
// https://github.com/ninja-build/ninja/blob/d4017a2b1ea642f12dabe05ec99b2a16c93e99aa/src/deps_log_test.cc

func TestReadWriteDepsLog(t *testing.T) {
	ctx := context.Background()
	fname := filepath.Join(t.TempDir(), "mydepslog")
	t1 := time.Unix(1, 0)
	t2 := time.Unix(2, 0)

	dl1, err := NewDepsLog(ctx, fname)
	if err != nil {
		t.Errorf("NewDepsLog(ctx, %s)=_, %v; want nil error", fname, err)
	}
	r, err := dl1.Record(ctx, "out.o", t1, []string{"foo.h", "bar.h"})
	if err != nil || !r {
		t.Errorf(`dl1.Record(ctx, "out.o", %v, []string{"foo.h", "bar.h"})=%t, %v; want true, nil error`, t1, r, err)
	}
	r, err = dl1.Record(ctx, "out2.o", t2, []string{"foo.h", "bar2.h"})
	if err != nil || !r {
		t.Errorf(`dl1.Record(ctx, "out2.o", %v, []string{"foo.h", "bar2.h"})=%t, %v; want true, nil error`, t2, r, err)
	}
	want := []string{"foo.h", "bar.h"}
	deps, mtime, err := dl1.Get(ctx, "out.o")
	if err != nil {
		t.Errorf(`d1.Get(ctx, "out.o")=_, _, %v; want _, _, nil error`, err)
	}
	if diff := cmp.Diff(deps, want); diff != "" {
		t.Errorf(`d1.Get(ctx, "out.o")=%v, _, _ mismatch (-got +want):\n%s`, deps, diff)
	}
	if !mtime.Equal(t1) {
		t.Errorf(`d1.Get(ctx, "out.o")=_, %v, _; want _, %v, _`, mtime, t1)
	}

	err = dl1.Close()
	if err != nil {
		t.Errorf("dl1.Close=%v; want nil error", err)
	}
	t.Logf("second deplog")
	dl2, err := NewDepsLog(ctx, fname)
	if err != nil {
		t.Errorf(`NewDepsLog(ctx, %s)=_, %v; want nil error`, fname, err)
	}
	defer func() {
		err := dl2.Close()
		if err != nil {
			t.Errorf("dl2.Close=%v; want nil error", err)
		}
	}()

	deps, mtime, err = dl2.Get(ctx, "out.o")
	if err != nil {
		t.Errorf(`dl2.Get(ctx, "out.o")=_, _, %v; want _, _, nil error`, err)
	}
	if diff := cmp.Diff(deps, want); diff != "" {
		t.Errorf(`dl2.Get(ctx, "out.o")=%v, _, _ mismatch (-got +want):\n%s`, deps, diff)
	}
	if !mtime.Equal(t1) {
		t.Errorf(`dl2.Get(ctx, "out.o")=_, %v, _; want _, %v, _`, mtime, t1)
	}
	want = []string{"foo.h", "bar2.h"}
	deps, mtime, err = dl2.Get(ctx, "out2.o")
	if err != nil {
		t.Errorf(`dl2.Get(ctx, "out2.o")=_, _, %v; want _, _, nil error`, err)
	}
	if diff := cmp.Diff(deps, want); diff != "" {
		t.Errorf(`dl2.Get(ctx, "out2.o")=%v, _, _ mismatch (-got +want):\n%s`, deps, diff)
	}
	if !mtime.Equal(t2) {
		t.Errorf(`dl2.Get(ctx, "out2.o")=_, %v, _; want _, %v, _`, mtime, t2)
	}
}
