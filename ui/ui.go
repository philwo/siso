// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package ui provides user interface functionalities.
package ui

import (
	"fmt"
	"strings"
)

// https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_(Select_Graphic_Rendition)_parameters
type SGRCode int

const (
	Bold SGRCode = iota
	Red
	Green
	Yellow
	BackgroundRed
	Reset
)

var sgrEscSeq = map[SGRCode]string{
	Bold:          "\033[1m",
	Red:           "\033[31;1m",
	Green:         "\033[32m",
	Yellow:        "\033[33m",
	BackgroundRed: "\033[41;37m",
	Reset:         "\033[0m",
}

func (s SGRCode) String() string {
	return sgrEscSeq[s]
}

// SGR formats s in SGR (select graphic rendition).
func SGR(n SGRCode, s string) string {
	return fmt.Sprintf("%s%s%s", n, s, Reset)
}

// StripANSIEscapeCodes strips ANSI escape codes.
func StripANSIEscapeCodes(s string) string {
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\033' {
			// not an escape code.
			sb.WriteByte(s[i])
			continue
		}
		// Only strip CSIs for now.
		if i+1 >= len(s) {
			break
		}
		if s[i+1] != '[' {
			// Not a CSI.
			continue
		}
		i += 2

		// Skip everything up to and including the next [a-zA-Z].
		for i < len(s) && !((s[i] >= 'a' && s[i] <= 'z') || s[i] >= 'A' && s[i] <= 'Z') {
			i++
		}
	}
	return sb.String()
}
