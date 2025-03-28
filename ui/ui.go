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
			msg = elideMiddle(msg, width)
		}
		if i > 0 {
			fmt.Fprintln(buf)
		}
		fmt.Fprint(buf, msg)
	}
}

func elideMiddle(msg string, width int) string {
	chrs := make([]byte, 0, len(msg))
	sgrs := make([]string, 0, len(msg))
	var sgr string
	hasSGR := false
	const escapeSeq = "\033["
	for i := 0; i < len(msg); i++ {
		if strings.HasPrefix(msg[i:], escapeSeq) {
			i += len(escapeSeq)
			j := strings.Index(msg[i:], "m")
			if j < 0 {
				// no SGR escape sequence?
				// TODO: handle this case correctly
				chrs = append(chrs, []byte(escapeSeq)...)
				chrs = append(chrs, []byte(msg[i:])...)
				hasSGR = false
				break
			}
			hasSGR = true
			sgr = msg[i : i+j]
			i += j
			continue
		}
		chrs = append(chrs, msg[i])
		sgrs = append(sgrs, sgr)
	}
	const elideMarker = "..."
	if len(chrs) < width {
		return msg
	}
	n := (width - (len(elideMarker) + 1)) / 2
	if len(chrs)+len(elideMarker) <= width-1 || n > len(chrs) {
		return msg
	}
	if !hasSGR {
		return msg[:n] + "..." + msg[len(msg)-n:]
	}
	sgr = ""
	var sb strings.Builder
	for i := 0; i < n; i++ {
		if sgrs[i] != sgr {
			sb.WriteString(escapeSeq)
			sb.WriteString(sgrs[i])
			sb.WriteString("m")
			sgr = sgrs[i]
		}
		sb.WriteByte(chrs[i])
	}
	if sgr != "" && sgr != "0" {
		sb.WriteString(escapeSeq + "0m")
	}
	sb.WriteString("...")
	sgr = "0"
	for i := len(chrs) - n; i < len(chrs); i++ {
		if sgrs[i] != sgr {
			sb.WriteString(escapeSeq)
			sb.WriteString(sgrs[i])
			sb.WriteString("m")
			sgr = sgrs[i]
		}
		sb.WriteByte(chrs[i])
	}
	if sgr != "" && sgr != "0" {
		sb.WriteString(escapeSeq + "0m")
	}
	return sb.String()
}

// https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_(Select_Graphic_Rendition)_parameters
const (
	Bold          = 1
	Red           = 31
	Green         = 32
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
