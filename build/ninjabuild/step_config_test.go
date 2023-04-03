// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjabuild

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"infra/build/siso/build"
	"infra/build/siso/hashfs"
)

func TestIndirectInputsFilter(t *testing.T) {
	for _, tc := range []struct {
		name       string
		ii         *IndirectInputs
		matches    []string
		nonmatches []string
	}{
		{
			name: "starAllMatch",
			ii: &IndirectInputs{
				Includes: []string{"*"},
			},
			matches: []string{
				"foo",
				"foo/bar",
			},
		},
		{
			name: "suffixMatch",
			ii: &IndirectInputs{
				Includes: []string{"*.json", "*.ts"},
			},
			matches: []string{
				"foo.json",
				"foo/bar.json",
				"foo.ts",
				"foo/bar.ts",
			},
			nonmatches: []string{
				"foo.js",
				"foo/bar.js",
				"foo.json/bar",
			},
		},
		{
			name: "baseMatch",
			ii: &IndirectInputs{
				Includes: []string{"foo-?.txt"},
			},
			matches: []string{
				"foo-1.txt",
				"bar/foo-1.txt",
			},
			nonmatches: []string{
				"foo-11.txt",
				"foo-1.txt/bar",
			},
		},
		{
			name: "pathMatch",
			ii: &IndirectInputs{
				Includes: []string{"foo/bar-?.txt"},
			},
			matches: []string{
				"foo/bar-1.txt",
				"foo/bar-2.txt",
			},
			nonmatches: []string{
				"foo/bar-11.txt",
				"x/foo/bar-1.txt",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := tc.ii.filter(ctx)
			for _, p := range tc.matches {
				if !f(ctx, p, false) {
					t.Errorf("f(ctx, %q, false)=false; want=true", p)
				}
			}
			for _, p := range tc.nonmatches {
				if f(ctx, p, false) {
					t.Errorf("f(ctx, %q, false)=true; want=false", p)
				}
			}
		})
	}
}

func TestStepConfigExpandInputs(t *testing.T) {
	ctx := context.Background()
	tdir := t.TempDir()
	err := os.MkdirAll(tdir, 0755)
	if err != nil {
		t.Fatal(err)
	}
	for _, fname := range []string{
		"out/Default/gen/out",
		"out/Default/gen/libfoo.so",
		"foo/bar",
		"foo/baz",
		"base/base.h",
		"component/a/1",
		"component/a/2",
		"component/b",
	} {
		pathname := filepath.Join(tdir, fname)
		err := os.MkdirAll(filepath.Dir(pathname), 0755)
		if err != nil {
			t.Fatal(err)
		}
		err = os.WriteFile(pathname, nil, 0644)
		if err != nil {
			t.Fatal(err)
		}
	}

	p := build.NewPath(tdir, "out/Default")
	t.Skip("TODO(b/266518906): hashfs is not implemented yet")
	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatalf("hashfs.New(...)=_, %v; want _, nil", err)
	}
	sc := StepConfig{
		InputDeps: map[string][]string{
			"./gen/out": {
				"./gen/libfoo.so",
			},
			"foo/bar": {
				"foo/baz",
				"base/base.h",
				"base/extra",
			},
			"component:component": {
				"component/a:a",
				"component/b",
			},
			"component/a:a": {
				"component/a/1",
				"component/a/2",
			},
		},
	}

	got := sc.ExpandInputs(ctx, p, hashFS, []string{
		"foo/bar",
		"out/Default/gen/out",
		"component:component",
		"extra/file",
	})

	want := []string{
		"base/base.h",
		"component/a/1",
		"component/a/2",
		"component/b",
		"foo/bar",
		"foo/baz",
		"out/Default/gen/libfoo.so",
		"out/Default/gen/out",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("sc.ExpandInputs(...); diff -want +got:\n%s", diff)
	}
}
