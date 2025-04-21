// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"bytes"
)

// panicLocation returns the first location just before runtime/panic.go
// from stacktrace buffer.
func panicLocation(buf []byte) []byte {
	i := bytes.Index(buf, []byte("\truntime/panic.go"))
	if i < 0 {
		return buf
	}
	buf = buf[i:]
	i = bytes.IndexByte(buf, '\n')
	if i < 0 {
		return buf
	}
	buf = buf[i+1:]
	i = bytes.IndexByte(buf, '\n')
	if i < 0 {
		return buf
	}
	nextLine := buf[i+1:]
	j := bytes.IndexByte(nextLine, '\n')
	if j < 0 {
		return buf
	}
	buf = buf[:i+1+j]
	return buf
}
