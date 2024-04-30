// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjautil

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TODO(b/267409605): At minimum have test coverage parity with
// https://github.com/ninja-build/ninja/blob/master/src/lexer_test.cc

func TestLexerVarValue(t *testing.T) {
	l := lexer{buf: []byte("plain text $var $VaR ${x}\n")}
	v, err := l.VarValue()
	if err != nil {
		t.Errorf("l.VarValue()=_, %v; want nil error", err)
	}
	want := "[plain text ][$var][ ][$VaR][ ][$x]"
	if got := v.String(); got != want {
		t.Errorf("l.VarValue=%s; want=%s", got, want)
	}
	s := v.RawString()
	if got, want := s, "plain text ${var} ${VaR} ${x}"; got != want {
		t.Errorf("RawString=%q; want=%q", got, want)
	}
	v2, err := parseEvalString(s)
	if err != nil {
		t.Errorf("parseEvalString(%q)=_ %v; want nil error", s, err)
	}
	if diff := cmp.Diff(v, v2, cmp.AllowUnexported(EvalString{}, tokenStr{})); diff != "" {
		t.Errorf("parse raw string diff -want +got:\n%s", diff)
	}
}

func TestLexerVarValue_Escapes(t *testing.T) {
	l := lexer{buf: []byte("$ $$ab c$: $\ncde\n")}
	v, err := l.VarValue()
	if err != nil {
		t.Errorf("l.VarValue=%v; want nil error", err)
	}
	want := "[ $ab c: cde]"
	if got := v.String(); got != want {
		t.Errorf("l.VarValue=%s; want=%s", got, want)
	}
	s := v.RawString()
	if got, want := s, " $$ab c: cde"; got != want {
		t.Errorf("RawString=%q; want=%q", got, want)
	}
	v2, err := parseEvalString(s)
	if err != nil {
		t.Errorf("parseEvalString(%q)=_ %v; want nil error", s, err)
	}
	if diff := cmp.Diff(v, v2, cmp.AllowUnexported(EvalString{}, tokenStr{})); diff != "" {
		t.Errorf("parse raw string diff -want +got:\n%s", diff)
	}
}

func TestLexerIdent(t *testing.T) {
	l := lexer{buf: []byte("foo BaR baz_123 foo-bar")}
	for _, want := range []string{
		"foo",
		"BaR",
		"baz_123",
		"foo-bar",
	} {
		tok, err := l.Ident()
		if err != nil || tok.String() != want {
			t.Errorf("Ident %q,%v; want %q", tok, err, want)
		}
	}
}

func TestLexerIdent_Curlies(t *testing.T) {
	l := lexer{buf: []byte("foo.dots $bar.dots ${bar.dots}\n")}
	tok, err := l.Ident()
	want := "foo.dots"
	if err != nil || tok.String() != want {
		t.Errorf("Ident=%q,%v; want %q", tok, err, want)
	}
	str, err := l.VarValue()
	want = "[$bar][.dots ][$bar.dots]"
	if err != nil || str.String() != want {
		t.Errorf("VarValue=%q, %v; want %q", str, err, want)
	}
}

func TestLexer_Error(t *testing.T) {
	l := lexer{buf: []byte("foo$\nbad $")}
	str, err := l.VarValue()
	if err == nil {
		t.Errorf("VarValue=%q, %v; want error", str, err)
	}
}

func TestLexer_CommentEOF(t *testing.T) {
	l := lexer{buf: []byte("# foo")}
	tok, err := l.Next()
	_, ok := tok.(tokenEOF)
	if err != nil || !ok {
		t.Errorf("Next=%T, %v; want %T, nil", tok, err, tokenEOF{})
	}
}

func TestLexer_Tabs(t *testing.T) {
	l := lexer{buf: []byte("    \tfoobar")}
	tok, err := l.Next()
	_, ok := tok.(tokenIndent)
	if err != nil || !ok {
		t.Errorf("Next=%T, %v; want %T, nil", tok, err, tokenIndent{})
	}
	tok, err = l.Next()
	if err == nil {
		t.Errorf("Next=%T, %v; want error", tok, err)
	}
}
