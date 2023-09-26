// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package makeutil provides utilities for make.
package makeutil

import (
	"bytes"
	"context"
	"io/fs"
	"strings"

	log "github.com/golang/glog"

	"infra/build/siso/o11y/clog"
)

// ParseDepsFile parses *.d file in fname on fsys.
func ParseDepsFile(ctx context.Context, fsys fs.FS, fname string) ([]string, error) {
	if fname == "" {
		return nil, nil
	}
	b, err := fs.ReadFile(fsys, fname)
	if err != nil {
		return nil, err
	}
	deps := ParseDeps(b)
	if log.V(1) {
		clog.Infof(ctx, "deps %s => %s", fname, deps)
	}
	return deps, nil
}

// ParseDeps parses deps and returns a list of inputs.
func ParseDeps(b []byte) []string {
	// deps contents
	// <output>: <input> ...
	// <input> is space separated
	// '\'+newline is space
	// '\'+space is escaped space (not separator)
	s := b
	var token string
	seen := make(map[string]bool)
	var inputs []string
depLines:
	for len(s) > 0 {
		// skip outputs until ':'
		i := bytes.IndexByte(s, ':')
		if i < 0 {
			break
		}
		// collect inputs
		for s = s[i+1:]; len(s) > 0; {
			token, s = nextToken(s)
			if token == "" {
				continue
			}
			if token == "\n" {
				continue depLines
			}
			if seen[token] {
				continue
			}
			seen[token] = true
			inputs = append(inputs, token)
		}
	}
	return inputs
}

func nextToken(s []byte) (string, []byte) {
	var sb strings.Builder
	// skip spaces
skipSpaces:
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && s[i+1] == '\n' {
			i++
			continue
		}
		if s[i] == '\\' && i+2 < len(s) && s[i+1] == '\r' && s[i+2] == '\n' {
			i += 2
			continue
		}
		switch s[i] {
		case '\n':
			return "\n", s[i+1:]
		case ' ', '\t', '\r':
			continue
		default:
			s = s[i:]
			break skipSpaces
		}
	}
	// extract next space not escaped
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			switch s[i] {
			case ' ':
				sb.WriteByte(s[i])
			case '\r', '\n':
				// '\'+newline is space
				return sb.String(), s[i-1:]
			default:
				sb.WriteByte('\\')
				sb.WriteByte(s[i])
			}
			continue
		}
		switch s[i] {
		case ' ', '\t', '\n', '\r':
			return sb.String(), s[i:]
		}
		sb.WriteByte(s[i])
	}
	return sb.String(), nil
}
