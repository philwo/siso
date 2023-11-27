// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ui

import (
	"bytes"
	"fmt"
	"os"
	"time"

	"golang.org/x/term"
)

type termSpinner struct {
	quit, done chan struct{}
	started    time.Time
	n          int
	msg        string
}

// Start starts the spinner.
func (s *termSpinner) Start(format string, args ...any) {
	s.started = time.Now()
	s.msg = fmt.Sprintf(format, args...)
	fmt.Printf("%s...", s.msg)
	s.quit = make(chan struct{})
	s.done = make(chan struct{})
	fmt.Printf(" ")
	go func() {
		defer close(s.done)
		for {
			select {
			case <-s.quit:
				return
			case <-time.After(1 * time.Second):
				const chars = `/-\|`
				fmt.Printf("\b%c", chars[s.n])
				s.n++
				if s.n >= len(chars) {
					s.n = 0
				}
			}
		}
	}()
}

// Stop stops the spinner.
func (s *termSpinner) Stop(err error) {
	close(s.quit)
	<-s.done
	d := time.Since(s.started)
	if err != nil {
		fmt.Printf("\r\033[K%6s %s failed %v\n", FormatDuration(d), s.msg, err)
		return
	}
	if d < DurationThreshold {
		// omit if duration is too short
		fmt.Printf("\r\033[K")
		return
	}
	fmt.Printf("\r\033[K%6s %s\n", FormatDuration(d), s.msg)
}

// Done finishes the spinner with message.
func (s *termSpinner) Done(format string, args ...any) {
	close(s.quit)
	<-s.done
	msg := fmt.Sprintf(format, args...)
	d := time.Since(s.started)
	fmt.Printf("\r\033[K%6s %s %s\n", FormatDuration(d), s.msg, msg)
}

// TermUI is a terminal-based UI.
type TermUI struct {
	width int
}

func (t *TermUI) init() {
	t.width, _, _ = term.GetSize(int(os.Stdout.Fd()))
}

// PrintLines implements the ui.ui interface.
// If msgs starts with \n, it will print from the current line.
// Otherwise, it will replace the last N lines, where N is len(msgs).
func (t *TermUI) PrintLines(msgs ...string) {
	var buf bytes.Buffer
	if len(msgs) > 0 && msgs[0] == "\n" {
		msgs = msgs[1:]
	} else {
		// Clear the last N lines, where N is len(msgs).
		for i := 0; i < len(msgs)-1; i++ {
			fmt.Fprintf(&buf, "\r\033[K\033[A")
		}
		fmt.Fprintf(&buf, "\r\033[K")

	}
	writeLinesMaxWidth(&buf, msgs, t.width)
	os.Stdout.Write(buf.Bytes())
}

// NewSpinner returns a terminal-based spinner.
func (TermUI) NewSpinner() spinner {
	return &termSpinner{}
}
