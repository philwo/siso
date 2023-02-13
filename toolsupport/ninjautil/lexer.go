// Copyright 2023 The Chromium Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjautil

import (
	"bytes"
	"fmt"
)

// Reference:
// https://github.com/ninja-build/ninja/blob/master/src/lexer.in.cc

type lexer struct {
	fname string
	buf   []byte
	last  int
	pos   int
}

type token interface{ token() }

type tokenString []byte

func (t tokenString) String() string { return string(t) }
func (tokenString) token()           {}

type tokenNewline struct{ tokenString }
type tokenIndent struct{ tokenString }
type tokenPool struct{ tokenString }
type tokenBuild struct{ tokenString }
type tokenRule struct{ tokenString }
type tokenDefault struct{ tokenString }
type tokenEq struct{ tokenString }
type tokenColon struct{ tokenString }
type tokenPipe2 struct{ tokenString }
type tokenPipe struct{ tokenString }
type tokenInclude struct{ tokenString }
type tokenSubninja struct{ tokenString }
type tokenIdent struct{ tokenString }

type tokenEOF struct{}

func (tokenEOF) token()         {}
func (tokenEOF) String() string { return "<EOF>" }

type lexerError struct {
	lexer *lexer
	pos   int
	msg   string
}

func (l *lexer) errorf(format string, args ...interface{}) error {
	return lexerError{
		lexer: l,
		pos:   l.pos,
		msg:   fmt.Sprintf(format, args...),
	}
}

func (e lexerError) Error() string {
	return fmt.Sprintf("%s:%d %s", e.lexer.fname, bytes.Count(e.lexer.buf[:e.pos], []byte("\n"))+1, e.msg)
}

func matchComment(buf []byte) int {
	i := 0
	for i < len(buf) && buf[i] == ' ' {
		i++
	}
	if buf[i] != '#' {
		return -1
	}
	i++
	for i < len(buf) && buf[i] != '\n' {
		i++
	}
	if i == len(buf) {
		return i
	}
	if buf[i] == '\n' {
		i++
	}
	return i
}

func matchNewline(buf []byte) int {
	i := 0
	for i < len(buf) && buf[i] == ' ' {
		i++
	}
	if buf[i] == '\r' {
		i++
	}
	if buf[i] == '\n' {
		return i + 1
	}
	return -1
}

func matchIndent(buf []byte) int {
	i := 0
	for i < len(buf) && buf[i] == ' ' {
		i++
	}
	return i
}

type charmap [8]uint32

func (m *charmap) set(ch byte) {
	(*m)[ch>>5] |= 1 << uint(ch&31)
}

func (m *charmap) contains(ch byte) bool {
	return (*m)[ch>>5]&(1<<uint(ch&31)) != 0
}

// [a-zA-Z0-9_.-]
var varnameChar charmap

func init() {
	for ch := byte('a'); ch <= 'z'; ch++ {
		varnameChar.set(ch)
	}
	for ch := byte('A'); ch <= 'Z'; ch++ {
		varnameChar.set(ch)
	}
	for ch := byte('0'); ch <= '9'; ch++ {
		varnameChar.set(ch)
	}
	varnameChar.set('_')
	varnameChar.set('.')
	varnameChar.set('-')
}

func matchKeyword(buf, kw []byte) int {
	if !bytes.HasPrefix(buf, kw) {
		return 0
	}
	if len(buf) == len(kw) {
		return len(buf)
	}
	// Not a keyword if followed by varnameChar.
	if varnameChar.contains(buf[len(kw)]) {
		return 0
	}
	return len(kw)
}

func matchVarname(buf []byte) int {
	n := len(buf)
	for i := 0; i < n; i++ {
		if !varnameChar.contains(buf[i]) {
			return i
		}
	}
	return n
}

func (l *lexer) eatWhitespace() {
	for {
		if len(l.buf[l.pos:]) == 0 {
			return
		}
		for l.pos < len(l.buf) && l.buf[l.pos] == ' ' {
			l.pos++
		}
		cur := l.buf[l.pos:]
		if len(cur) == 0 {
			return
		}
		if bytes.HasPrefix(cur, []byte("$\r\n")) {
			l.pos += 3
			continue
		}
		if bytes.HasPrefix(cur, []byte("$\n")) {
			l.pos += 2
			continue
		}
		return
	}
}

func (l *lexer) Next() (token, error) {
	var t token
	var s int
loop:
	for {
		s = l.pos
		cur := l.buf[l.pos:]
		if len(cur) == 0 {
			return tokenEOF{}, nil
		}
		if i := matchComment(cur); i > 0 {
			l.pos += i
			continue
		}
		if i := matchNewline(cur); i > 0 {
			t = tokenNewline{tokenString(cur[:i])}
			l.pos += i
			break loop
		}
		if i := matchIndent(cur); i > 0 {
			t = tokenIndent{tokenString(cur[:i])}
			l.pos += i
			break loop
		}
		// TODO(b/254182269): Consider defining these known keywords.
		if i := matchKeyword(cur, []byte("build")); i > 0 {
			t = tokenBuild{tokenString(cur[:i])}
			l.pos += i
			break loop
		}
		if i := matchKeyword(cur, []byte("pool")); i > 0 {
			t = tokenPool{tokenString(cur[:i])}
			l.pos += i
			break loop
		}
		if i := matchKeyword(cur, []byte("rule")); i > 0 {
			t = tokenRule{tokenString(cur[:i])}
			l.pos += i
			break loop
		}
		if i := matchKeyword(cur, []byte("default")); i > 0 {
			t = tokenDefault{tokenString(cur[:i])}
			l.pos += i
			break loop
		}
		if bytes.HasPrefix(cur, []byte("=")) {
			t = tokenEq{tokenString(cur[:len("=")])}
			l.pos += len("=")
			break loop
		}
		if bytes.HasPrefix(cur, []byte(":")) {
			t = tokenColon{tokenString(cur[:len(":")])}
			l.pos += len(":")
			break loop
		}
		if bytes.HasPrefix(cur, []byte("||")) {
			t = tokenPipe2{tokenString(cur[:len("||")])}
			l.pos += len("||")
			break loop
		}
		if bytes.HasPrefix(cur, []byte("|")) {
			t = tokenPipe{tokenString(cur[:len("|")])}
			l.pos += len("|")
			break loop
		}
		if i := matchKeyword(cur, []byte("include")); i > 0 {
			t = tokenInclude{tokenString(cur[:i])}
			l.pos += i
			break loop
		}
		if i := matchKeyword(cur, []byte("subninja")); i > 0 {
			t = tokenSubninja{tokenString(cur[:i])}
			l.pos += i
			break loop
		}
		if i := matchVarname(cur); i > 0 {
			t = tokenIdent{tokenString(cur[:i])}
			l.pos += i
			break loop
		}
		return nil, l.errorf("unknown token")
	}
	l.last = s
	switch t.(type) {
	case tokenNewline, tokenEOF:
	default:
		l.eatWhitespace()
	}
	return t, nil
}
