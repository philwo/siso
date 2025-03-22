// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ui

import (
	"fmt"
	"time"

	"github.com/charmbracelet/log"
)

type logSpinner struct {
	started time.Time
}

// Start implements the ui.spinner interface.
// Because a log-based UI cannot support an animated spinner, this is used only to report spinner completion.
func (l *logSpinner) Start(format string, args ...any) {
	l.started = time.Now()
	log.Infof(format, args...)
}

// Stop implements the ui.spinner interface.
// Because a log-based UI cannot support an animated spinner, this is used to report how long the spinner operation took to complete.
func (l *logSpinner) Stop(err error) {
	if err != nil {
		log.Warnf("-> failed %s %v", time.Since(l.started), err)
		return
	}
	log.Infof("-> done %s", time.Since(l.started))
}

// Done finishes the spinner with message.
func (l *logSpinner) Done(format string, args ...any) {
	log.Infof("-> %s %s", fmt.Sprintf(format, args...), time.Since(l.started))
}

// LogUI is a log-based UI.
type LogUI struct{}

// PrintLines implements the ui.ui interface.
// Because a log-based UI cannot support erasing previous lines, msgs will be printed as-is.
func (LogUI) PrintLines(msgs ...string) {
	log.Helper()
	for i := range msgs {
		log.Info(StripANSIEscapeCodes(msgs[i]))
	}
}

// NewSpinner returns an implementation of ui.spinner.
func (LogUI) NewSpinner() spinner {
	return &logSpinner{}
}
