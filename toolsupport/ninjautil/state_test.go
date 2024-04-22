// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjautil

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestState_Targets(t *testing.T) {
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
			name:    "missing-header^",
			input:   input,
			args:    []string{"../../foo/bar.h^"},
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := NewState()
			p := NewManifestParser(state)
			err := p.parse(context.Background(),
				&lexer{
					fname: "input",
					buf:   []byte(tc.input),
				},
			)
			if err != nil {
				t.Fatalf("parse %v", err)
			}
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
