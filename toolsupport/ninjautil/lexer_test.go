// Copyright 2023 The Chromium Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjautil

import (
	"testing"
)

// TODO(b/267409605): At minimum have test coverage parity with
// https://github.com/ninja-build/ninja/blob/master/src/lexer_test.cc

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
