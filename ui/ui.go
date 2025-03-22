// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package ui provides user interface functionalities.
package ui

import (
	"fmt"
	"strings"
	"unicode"
)

// Spinner controls spinner.
type Spinner interface {
	// Start starts the spinner with the specified formatted string.
	Start(format string, args ...any)
	// Stop stops the spinner, outputting an error if provided.
	Stop(err error)
	// Done finishes the spinner with message.
	Done(format string, args ...any)
}

// UI is a user interface.
type UI interface {
	// NewSpinner returns a new spinner.
	NewSpinner() Spinner

	// Infof reports info level.
	Infof(string, ...any)

	// Warningf reports warning level.
	Warningf(string, ...any)

	// Errorf reports error level.
	Errorf(string, ...any)
}

// Default holds the default UI interface.
// Making changes to this variable after init is undefined behavior.
// UI implementations are currently not expected to handle being changed.
var Default UI

func init() {
	Default = &LogUI{}
}

// IsTerminal returns whether currently using a terminal UI.
func IsTerminal() bool {
	return false
}

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

		// Loop while current char is NOT an ASCII letter.
		for i < len(s) && !unicode.IsLetter(rune(s[i])) {
			i++
		}
	}
	return sb.String()
}
