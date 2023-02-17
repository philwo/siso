// Copyright 2023 The Chromium Authors. All rights reserved.
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

var (
	// IsTerminal indicates the stdout is terminal or not.
	IsTerminal = term.IsTerminal(int(os.Stdout.Fd()))
	width      int
)

func init() {
	if !IsTerminal {
		return
	}
	width, _, _ = term.GetSize(int(os.Stdout.Fd()))
}

// PrintLines prints message lines.
// If msgs starts with \n, it will print from the current line.
// Otherwise, it will replaces the last N lines, where N is len(msgs).
func PrintLines(msgs ...string) {
	if !IsTerminal {
		os.Stdout.Write([]byte(strings.Join(msgs, "\t") + "\n"))
		return
	}
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
			fmt.Fprintln(&buf)
		}
		fmt.Fprint(&buf, msg)
	}
	os.Stdout.Write(buf.Bytes())
}
