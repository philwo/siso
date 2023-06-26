// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package ui provides user interface functionalities.
package ui

import (
	"bytes"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

type spinner interface {
	// Start starts the spinner with the specified formatted string.
	Start(format string, args ...any)
	// Stop stops the spinner, outputting an error if provided.
	Stop(err error)
}

// UI is a user interface.
type UI interface {
	// PrintLines prints message lines.
	// If msgs starts with \n, it will print from the current line.
	// Otherwise, it will replaces the last N lines, where N is len(msgs).
	PrintLines(msgs ...string)
	// NewSpinner returns a new spinner.
	NewSpinner() spinner
}

// Default holds the default UI interface.
// Making changes to this variable after init is undefined behavior.
// UI implementations are currently not expected to handle being changed.
var Default UI

func init() {
	if term.IsTerminal(int(os.Stdout.Fd())) {
		termUI := &TermUI{}
		termUI.init()
		Default = termUI
	} else {
		Default = &LogUI{}
	}
}

// IsTerminal returns whether currently using a terminal UI.
func IsTerminal() bool {
	_, ok := Default.(*TermUI)
	return ok
}

func writeLinesMaxWidth(buf *bytes.Buffer, msgs []string, width int) {
	for i, msg := range msgs {
		if msg == "" {
			continue
		}
		// Truncate in middle if too long, unless terminated with newline.
		// If printing last message, don't truncate if any newline.
		if width > 4 && len(msg)+3 > width-1 && ((width-4)/2) < len(msg) &&
			((i < len(msgs)-1 && !strings.Contains(msg[:len(msg)-1], "\n")) ||
				(i == len(msgs)-1 && !strings.Contains(msg, "\n"))) {
			n := (width - 4) / 2
			msg = msg[:n] + "..." + msg[len(msg)-n:]
		}
		if i > 0 {
			fmt.Fprintln(buf)
		}
		fmt.Fprint(buf, msg)
	}
}

// https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_(Select_Graphic_Rendition)_parameters
const (
	BackgroundRed = 41
)

// SGR formats s in SGR (select graphic rendition).
func SGR(n int, s string) string {
	return fmt.Sprintf("\033[%dm%s\033[0m", n, s)
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
