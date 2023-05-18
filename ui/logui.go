// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ui

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// LogUi is a log-based UI.
type LogUi struct {
	spinnerStarted time.Time
}

func (l *LogUi) Init() {}

func (l *LogUi) PrintLines(msgs ...string) {
	os.Stdout.Write([]byte(strings.Join(msgs, "\t") + "\n"))
}

func (l *LogUi) StartSpinner(format string, args ...any) {
	l.spinnerStarted = time.Now()
	fmt.Printf(format, args...)
}

func (l *LogUi) StopSpinner(err error) {
	if err != nil {
		fmt.Printf("\bfailed %s %v\n", time.Since(l.spinnerStarted), err)
		return
	}
	fmt.Printf("\bdone %s\n", time.Since(l.spinnerStarted))
}
