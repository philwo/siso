// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjautil

import (
	"hash/maphash"
	"sync"
	"testing"
)

func TestBigMap(t *testing.T) {
	bm := bigMap{
		seed: maphash.MakeSeed(),
	}

	var wg sync.WaitGroup
	wg.Add(100)
	type result struct {
		foo, bar *Node
	}
	results := make([]result, 100)
	for i := range results {
		go func(r *result) {
			defer wg.Done()
			for range 1000 {
				n := bm.node([]byte("foo"))
				if n == nil {
					t.Errorf("bm.node(%q)=nil; want non nil", "foo")
					return
				}
				if n.path != "foo" {
					t.Errorf("bm.node(%q).path=%q; want %q", "foo", n.path, "foo")
				}
				if r.foo == nil {
					r.foo = n
				}
				n = bm.node([]byte("foo"))
				if n != r.foo {
					t.Errorf("bm.node(%q)=%p; want %p", "foo", n, r.foo)
				}
				n = bm.node([]byte("bar"))
				if n == nil {
					t.Errorf("bm.node(%q)=nil; want non nil", "bar")
					return
				}
				if n.path != "bar" {
					t.Errorf("bm.node(%q).path=%q; want %q", "bar", n.path, "bar")
				}
				if r.bar == nil {
					r.bar = n
				}
				if n != r.bar {
					t.Errorf("bm.node(%q)=%p; want %p", "bar", n, r.bar)
				}
				n, ok := bm.lookup("foo")
				if n != r.foo || !ok {
					t.Errorf("bm.lookup(%q)=%p, %t; want %p, true", "foo", n, ok, r.foo)
				}
				n, ok = bm.lookup("bar")
				if n != r.bar || !ok {
					t.Errorf("bm.lookup(%q)=%p, %t; want %p, true", "bar", n, ok, r.bar)
				}
				n, ok = bm.lookup("baz")
				if n != nil || ok {
					t.Errorf("bm.lookup(%q)=%p, %t; want false", "baz", n, ok)
				}

			}
		}(&results[i])
	}
	wg.Wait()
	for i := range results {
		if results[i].foo != results[0].foo {
			t.Errorf("%d: foo=%p want=%p", i, results[i].foo, results[0].foo)
		}
		if results[i].bar != results[0].bar {
			t.Errorf("%d: bar=%p want=%p", i, results[i].bar, results[0].bar)
		}
	}
}
