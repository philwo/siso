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

// spinner manages a spinner for long operation.
type spinner struct {
	quit, done chan struct{}
	started    time.Time
	n          int
}

// Start starts the spinner.
func (s *spinner) Start(format string, args ...any) {
	s.started = time.Now()
	fmt.Printf(format, args...)
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
func (s *spinner) Stop(err error) {
	close(s.quit)
	<-s.done
	if err != nil {
		fmt.Printf("\bfailed %s %v\n", time.Since(s.started), err)
		return
	}
	fmt.Printf("\bdone %s\n", time.Since(s.started))
}

// TermUi is a terminal-based UI.
type TermUi struct {
	width   int
	spinner spinner
}

func (t *TermUi) Init() {
	t.width, _, _ = term.GetSize(int(os.Stdout.Fd()))
}

func (t *TermUi) PrintLines(msgs ...string) {
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

func (t *TermUi) StartSpinner(format string, args ...any) {
	t.spinner.Start(format, args...)
}

func (t *TermUi) StopSpinner(err error) {
	t.spinner.Stop(err)
}
