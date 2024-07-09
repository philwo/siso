// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjautil

import (
	"context"
	"testing"
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
