// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjautil

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestParser_Empty(t *testing.T) {
	state := NewState()
	p := NewManifestParser(state)
	_, err := p.parse(context.Background(), &lexer{fname: "input"})
	if err != nil {
		t.Errorf("parse %v", err)
	}
}

func TestParser_Rules(t *testing.T) {
	state := NewState()
	p := NewManifestParser(state)
	_, err := p.parse(context.Background(),
		&lexer{
			fname: "input",
			buf: []byte(`rule cat
  command = cat $in > $out
rule date
  command = date > $out
build result: cat in_1.cc in-2.O
`),
		})
	if err != nil {
		t.Errorf("parse %v", err)
	}
	rule, ok := p.rules.rules["cat"]
	if !ok {
		t.Errorf("missing cat rule %v", p.rules.rules)
	}
	if rule.Name() != "cat" {
		t.Errorf("rule.Name=%q; want=%q", rule.Name(), "cat")
	}
	cmd, ok := rule.Binding("command")
	if !ok {
		t.Errorf("rule cat missing command")
	}
	want := "[cat ][$in][ > ][$out]"
	if cmd.String() != want {
		t.Errorf("rule cat command=%q; want=%q", cmd, want)
	}
	rule, ok = p.rules.rules["date"]
	if !ok {
		t.Errorf("missing date rule %v", p.rules.rules)
	}
	if rule.Name() != "date" {
		t.Errorf("rule.Name=%q; want=%q", rule.Name(), "date")
	}

}

func TestParser_Dupbuild_Error(t *testing.T) {
	state := NewState()
	p := NewManifestParser(state)
	_, err := p.parse(context.Background(),
		&lexer{
			fname: "build.ninja",
			buf: []byte(`
rule cat
  command = cat $in > $out
build b: cat a
build b: cat c
`),
		})
	want := p.lexer.errorf("multiple rules generate b")
	if err != want {
		t.Errorf("p.parse() got: %v; want: %v", err, want)
	}
}

func TestParser_ConcurrentSubninja(t *testing.T) {
	origLoaderConcurrency := loaderConcurrency
	loaderConcurrency = 8
	defer func() { loaderConcurrency = origLoaderConcurrency }()
	ctx := context.Background()
	state := NewState()
	p := NewManifestParser(state)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	defer func() {
		err = os.Chdir(wd)
		if err != nil {
			t.Fatal(err)
		}
	}()
	err = os.Chdir(dir)
	if err != nil {
		t.Fatal(err)
	}

	write := func(fname, content string) {
		t.Helper()
		err := os.MkdirAll(filepath.Dir(fname), 0755)
		if err != nil {
			t.Fatal(err)
		}
		err = os.WriteFile(fname, []byte(content), 0644)
		if err != nil {
			t.Fatal(err)
		}
	}

	write("build.ninja", `
subninja a/build.ninja
subninja b/build.ninja
subninja c/build.ninja
subninja d/build.ninja
subninja e/build.ninja
subninja f/build.ninja
subninja g/build.ninja
subninja h/build.ninja
subninja i/build.ninja
`)

	for _, d := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"} {
		write(fmt.Sprintf("%s/build.ninja", d), fmt.Sprintf(`
subninja %[1]s/a/build.ninja
subninja %[1]s/a/build.ninja
subninja %[1]s/b/build.ninja
subninja %[1]s/c/build.ninja
subninja %[1]s/d/build.ninja
subninja %[1]s/e/build.ninja
subninja %[1]s/f/build.ninja
subninja %[1]s/g/build.ninja
subninja %[1]s/h/build.ninja
subninja %[1]s/i/build.ninja
`, d))
		write(fmt.Sprintf("%s/a/build.ninja", d), "")
		write(fmt.Sprintf("%s/b/build.ninja", d), "")
		write(fmt.Sprintf("%s/c/build.ninja", d), "")
		write(fmt.Sprintf("%s/d/build.ninja", d), "")
		write(fmt.Sprintf("%s/e/build.ninja", d), "")
		write(fmt.Sprintf("%s/f/build.ninja", d), "")
		write(fmt.Sprintf("%s/g/build.ninja", d), "")
		write(fmt.Sprintf("%s/h/build.ninja", d), "")
		write(fmt.Sprintf("%s/i/build.ninja", d), "")
	}

	err = p.Load(ctx, "build.ninja")
	if err != nil {
		t.Fatal(err)
	}
}

func TestParser_Validation(t *testing.T) {
	ctx := context.Background()
	state := NewState()
	p := NewManifestParser(state)
	_, err := p.parse(ctx,
		&lexer{
			fname: "input",
			buf: []byte(`
rule cat
   command = cat $in > $out
build foo: cat bar |@ baz baz2
`),
		})
	if err != nil {
		t.Errorf("parse %v", err)
	}
	p.state.nodes, p.state.paths = p.state.nodeMap.freeze(ctx)

	node, ok := state.LookupNodeByPath("foo")
	if !ok {
		t.Fatalf("foo not found")
	}
	edge, ok := node.InEdge()
	if !ok {
		t.Fatalf("no inEdge of foo")
	}
	validations := edge.Validations()
	var got []string
	for _, v := range validations {
		got = append(got, v.Path())
	}
	want := []string{"baz", "baz2"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("validations for foo: -want +got:\n%s", diff)
	}
}
