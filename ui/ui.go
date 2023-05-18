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

type ui interface {
	// Init inits the UI.
	Init()
	// PrintLines prints message lines.
	// If msgs starts with \n, it will print from the current line.
	// Otherwise, it will replaces the last N lines, where N is len(msgs).
	PrintLines(msgs ...string)
	// StartSpinner starts the spinner with the specified formatted string.
	StartSpinner(format string, args ...any)
	// StopSpinner stops the spinner, outputting an error if provided.
	StopSpinner(err error)
}

// CurrentUi holds the current UI interface. This is exposed to allow tests to change the UI.
// Making changes to the current UI during operations is undefined behavior.
// Implementations of UIs are not currently expected to safely handle being changed mid-operation.
var CurrentUi ui

func init() {
	if term.IsTerminal(int(os.Stdout.Fd())) {
		CurrentUi = &TermUi{}
	} else {
		CurrentUi = &LogUi{}
	}
	CurrentUi.Init()
}

// IsTerminal returns whether currently using a terminal UI.
func IsTerminal() bool {
	_, ok := CurrentUi.(*TermUi)
	return ok
}

// PrintLines prints message lines.
// If msgs starts with \n, it will print from the current line.
// Otherwise, it will replaces the last N lines, where N is len(msgs).
func PrintLines(msgs ...string) {
	CurrentUi.PrintLines(msgs...)
}

// Spinner is a legacy shim for spinner operations.
type Spinner struct{}

// Start starts the spinner.
func (s *Spinner) Start(format string, args ...any) {
	CurrentUi.StartSpinner(format, args...)
}

// Stop stops the spinner.
func (s *Spinner) Stop(err error) {
	CurrentUi.StopSpinner(err)
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
