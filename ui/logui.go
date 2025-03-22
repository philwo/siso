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

// NewSpinner returns an implementation of ui.spinner.
func (LogUI) NewSpinner() Spinner {
	return &logSpinner{}
}

// Infof reports to stdout, stripping ansi escape sequence.
func (LogUI) Infof(format string, args ...any) {
	log.Helper()
	log.Info(StripANSIEscapeCodes(fmt.Sprintf(format, args...)))
}

// Warningf reports to stderr, stripping ansi escape sequence.
func (LogUI) Warningf(format string, args ...any) {
	log.Helper()
	log.Warn(StripANSIEscapeCodes(fmt.Sprintf(format, args...)))
}

// Errorf reports to stderr, stripping ansi escape sequence.
func (LogUI) Errorf(format string, args ...any) {
	log.Helper()
	log.Error(StripANSIEscapeCodes(fmt.Sprintf(format, args...)))
}
