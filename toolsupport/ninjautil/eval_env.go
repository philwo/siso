// Copyright 2023 The Chromium Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjautil

import (
	"fmt"
	"strings"
)

type tokenStrType int

const (
	tokenStrLiteral = iota
	tokenStrVariable
)

type tokenStr struct {
	t tokenStrType
	s []byte
}

// EvalString is an eval string.
type EvalString struct {
	s []tokenStr
}

func (e *EvalString) addLiteral(p []byte) {
	e.s = append(e.s, tokenStr{t: tokenStrLiteral, s: p})
}

func (e *EvalString) addVar(p []byte) {
	e.s = append(e.s, tokenStr{t: tokenStrVariable, s: p})
}

// String returns parsed eval string.
func (e EvalString) String() string {
	var sb strings.Builder
	literal := false
	for _, t := range e.s {
		switch t.t {
		case tokenStrLiteral:
			if !literal {
				sb.WriteString("[")
			}
			sb.Write(t.s)
			literal = true
		case tokenStrVariable:
			if literal {
				sb.WriteString("]")
				literal = false
			}
			fmt.Fprintf(&sb, "[$%s]", t.s)
		}
	}
	if literal {
		sb.WriteString("]")
	}
	return sb.String()
}
