// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build windows

package runtimex

import (
	"syscall"

	"golang.org/x/sys/windows"
)

func getproccount() int {
	r0, _, _ := syscall.SyscallN(windows.NewLazySystemDLL("kernel32.dll").NewProc("GetActiveProcessorCount").Addr(), 1, uintptr(0xFFFF), 0, 0)
	return int(r0)
}
