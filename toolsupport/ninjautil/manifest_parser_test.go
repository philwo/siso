// Copyright 2023 The Chromium Authors. All rights reserved.
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
	err := p.parse(context.Background(), &lexer{fname: "input"})
	if err != nil {
		t.Errorf("parse %v", err)
	}
}

func TestParser_Rules(t *testing.T) {
	state := NewState()
	p := NewManifestParser(state)
	err := p.parse(context.Background(),
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
	rule, ok := state.bindings.rules["cat"]
	if !ok {
		t.Errorf("missing cat rule %v", state.bindings.rules)
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
	rule, ok = state.bindings.rules["date"]
	if !ok {
		t.Errorf("missing date rule %v", state.bindings.rules)
	}
	if rule.Name() != "date" {
		t.Errorf("rule.Name=%q; want=%q", rule.Name(), "date")
	}

}
