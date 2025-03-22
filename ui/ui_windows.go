// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ui

import (
	"os"

	"github.com/golang/glog"
	"golang.org/x/sys/windows"
)

var consoleMode uint32

// Init initializes the stdout settings.
// It enables virtual terminal processing for ANSI escape sequence.
func Init() {
	var mode uint32
	err := windows.GetConsoleMode(windows.Handle(os.Stdout.Fd()), &mode)
	if err != nil {
		glog.Warningf("GetConsoleMode %v", err)
		return
	}
	glog.Infof("console mode=0x%x", mode)
	consoleMode = mode
	if mode&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING != 0 {
		return
	}
	mode |= windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING
	err = windows.SetConsoleMode(windows.Handle(os.Stdout.Fd()), mode)
	glog.Infof("set console mode 0x%0x: %v", mode, err)
	if err != nil {
		glog.Errorf("SetConsoleMode 0x%x: %v", mode, err)
	}
}

// Restore restores the stdout settings.
func Restore() {
	if consoleMode == 0 {
		return
	}
	err := windows.SetConsoleMode(windows.Handle(os.Stdout.Fd()), consoleMode)
	if err != nil {
		glog.Errorf("SetConsoleMode 0x%x: %v", consoleMode, err)
	}
}
