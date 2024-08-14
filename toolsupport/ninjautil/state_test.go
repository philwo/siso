// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjautil

import (
	"context"
	"hash/maphash"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestState_Targets(t *testing.T) {
	dir := t.TempDir()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir = filepath.Join(dir, "out/siso")
	err = os.MkdirAll(dir, 0755)
	if err != nil {
		t.Fatal(err)
	}

	err = os.Chdir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		err := os.Chdir(wd)
		if err != nil {
			t.Error(err)
		}
	}()

	err = os.MkdirAll(filepath.Join(dir, "../../foo"), 0755)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(filepath.Join(dir, "../../foo/foo.cc"), []byte(`
#include "foo/foo.h"
#include "foo/foo_util.h"
`), 0644)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(filepath.Join(dir, "../../foo/foo.h"), nil, 0644)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(filepath.Join(dir, "../../foo/foo_util.h"), nil, 0644)
	if err != nil {
		t.Fatal(err)
	}

	inputNoDefault := `
rule cxx
  command = clang++ -c ${in} ${out}

build obj/foo.o: cxx ../../foo/foo.cc

build foo: phony obj/foo.o
build all: phony foo
build full: phony foo obj/foo.o
`

	input := inputNoDefault + `
default all
`
	for _, tc := range []struct {
		name    string
		input   string
		args    []string
		want    []string
		wantErr bool
	}{
		{
			name:  "no_default",
			input: inputNoDefault,
			args:  nil,
			want:  []string{"all", "full"},
		},
		{
			name:  "default",
			input: input,
			args:  nil,
			want:  []string{"all"},
		},
		{
			name:  "all_given",
			input: input,
			args:  []string{"all"},
			want:  []string{"all"},
		},
		{
			name:  "foo_given",
			input: input,
			args:  []string{"foo"},
			want:  []string{"foo"},
		},
		{
			name:  "dotslash",
			input: input,
			args:  []string{"./foo"},
			want:  []string{"foo"},
		},
		{
			name:  "dotslash_extra",
			input: input,
			args:  []string{".////obj////foo.o"},
			want:  []string{"obj/foo.o"},
		},
		{
			name:    "wrong_target",
			input:   input,
			args:    []string{"bar"},
			wantErr: true,
		},
		{
			name:  "special^",
			input: input,
			args:  []string{"../../foo/foo.cc^"},
			want:  []string{"obj/foo.o"},
		},
		{
			name:    "missing^",
			input:   input,
			args:    []string{"../../foo/bar.cc^"},
			wantErr: true,
		},
		{
			name:  "header^",
			input: input,
			args:  []string{"../../foo/foo.h^"},
			want:  []string{"obj/foo.o"},
		},
		{
			name:  "dotslash^",
			input: input,
			args:  []string{"./../../foo/foo.h^"},
			want:  []string{"obj/foo.o"},
		},
		{
			name:  "dotslash_extra^",
			input: input,
			args:  []string{"./////..///////../foo/foo.h^"},
			want:  []string{"obj/foo.o"},
		},
		{
			name:    "missing-header^",
			input:   input,
			args:    []string{"../../foo/bar.h^"},
			wantErr: true,
		},
		{
			name:  "header^-include",
			input: input,
			args:  []string{"../../foo/foo_util.h^"},
			want:  []string{"obj/foo.o"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			state := NewState()
			p := NewManifestParser(state)
			_, err := p.parse(context.Background(),
				&lexer{
					fname: "input",
					buf:   []byte(tc.input),
				},
			)
			if err != nil {
				t.Fatalf("parse %v", err)
			}
			p.state.nodes, p.state.paths = p.state.nodeMap.freeze(ctx)
			nodes, err := state.Targets(tc.args)
			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Errorf("state.Targets(%q)=%q, %v; want %q; err=%t", tc.args, nodes, err, tc.want, tc.wantErr)
			}
			var got []string
			for _, n := range nodes {
				got = append(got, n.Path())
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("state.Targets(%q) diff -want +got:\n%s", tc.args, diff)
			}
		})
	}
}

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
	for i := range 100 {
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
	for i := range 100 {
		if results[i].foo != results[0].foo {
			t.Errorf("%d: foo=%p want=%p", i, results[i].foo, results[0].foo)
		}
		if results[i].bar != results[0].bar {
			t.Errorf("%d: bar=%p want=%p", i, results[i].bar, results[0].bar)
		}
	}
}
