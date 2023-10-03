// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package msvcutil

import (
	"bytes"
)

// msvc may localize text, but we assume developers don't use that.
const depsPrefix = "Note: including file: "

// ParseShowIncludes parses /showIncludes outputs, and returns a list of inputs and other outputs.
func ParseShowIncludes(b []byte) ([]string, []byte) {
	// showIncludes contents
	//  Note: including file:  <pathname>\r\n
	//
	// other lines will be normal stdout/stderr (e.g. compiler error message)
	var deps []string
	var outs []byte
	s := b
	for len(s) > 0 {
		line := s
		i := bytes.IndexAny(s, "\r\n")
		if i >= 0 {
			line = line[:i]
			s = s[i:]
		} else {
			s = nil
		}
		if bytes.HasPrefix(line, []byte(depsPrefix)) {
			line = bytes.TrimPrefix(line, []byte(depsPrefix))
			line = bytes.TrimSpace(line)
			deps = append(deps, string(line))
			if bytes.HasPrefix(s, []byte("\r")) {
				s = s[1:]
			}
			if bytes.HasPrefix(s, []byte("\n")) {
				s = s[1:]
			}
			continue
		}
		outs = append(outs, line...)
		if bytes.HasPrefix(s, []byte("\r")) {
			outs = append(outs, '\r')
			s = s[1:]
		}
		if bytes.HasPrefix(s, []byte("\n")) {
			outs = append(outs, '\n')
			s = s[1:]
		}
	}
	return deps, outs
}
