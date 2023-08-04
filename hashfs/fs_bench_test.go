// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package hashfs

import (
	"context"
	"io/fs"
	"testing"
)

func BenchmarkDirectoryLookup(b *testing.B) {
	ctx := context.Background()
	root := &directory{}
	fname := "/b/s/w/ir/cache/builder/src/out/siso/gen"
	b.Run("miss", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, _, ok := root.lookup(ctx, fname)
			if ok {
				b.Fatalf("lookup(ctx, %q)=_, _, %t; want false", fname, ok)
			}
		}
	})
	e := &entry{err: fs.ErrNotExist}
	root.store(ctx, fname, e)
	b.Run("ok", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, _, ok := root.lookup(ctx, fname)
			if !ok {
				b.Fatalf("lookup(ctx, %q)=_, _, %t; want true", fname, ok)
			}
		}
	})
}

// test to make sure keep allocations under
// allocations that was measured by the above benchmark.
// fs_test.go is eternal test, but this is internal test.
func TestDirectoryLookup(t *testing.T) {
	ctx := context.Background()
	root := &directory{}
	fname := "/b/s/w/ir/cache/builder/src/out/siso/gen"

	t.Run("miss", func(t *testing.T) {
		avg := testing.AllocsPerRun(1000, func() {
			_, _, ok := root.lookup(ctx, fname)
			if ok {
				t.Fatalf("lookup(ctx, %q)=_, _, %t; want false", fname, ok)
			}
		})
		if avg != 0 {
			t.Errorf("alloc=%f; want 0", avg)
		}
	})

	e := &entry{err: fs.ErrNotExist}
	root.store(ctx, fname, e)
	t.Run("ok", func(t *testing.T) {
		avg := testing.AllocsPerRun(1000, func() {
			_, _, ok := root.lookup(ctx, fname)
			if !ok {
				t.Fatalf("lookup(ctx, %q)=_, _, %t; want true", fname, ok)
			}
		})
		if avg != 0 {
			t.Errorf("alloc=%f; want 0", avg)
		}
	})
}
