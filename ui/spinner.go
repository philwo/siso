// Copyright 2023 The Chromium Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ui

import (
	"fmt"
	"time"
)

// Spinner manages a spinner for long operation.
type Spinner struct {
	quit, done chan struct{}
	started    time.Time
	n          int
}

// Start starts the spinner.
func (s *Spinner) Start(format string, args ...interface{}) {
	s.started = time.Now()
	fmt.Printf(format, args...)
	if !IsTerminal {
		return
	}
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
func (s *Spinner) Stop(err error) {
	if IsTerminal {
		close(s.quit)
		<-s.done
	}
	if err != nil {
		fmt.Printf("\bfailed %s %v\n", time.Since(s.started), err)
		return
	}
	fmt.Printf("\bdone %s\n", time.Since(s.started))
}
