// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build unix

package localexec

import (
	"os/exec"
	"syscall"

	epb "infra/build/siso/execute/proto"
)

func rusage(cmd *exec.Cmd) *epb.Rusage {
	if u, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage); ok {
		return &epb.Rusage{
			MaxRss: u.Maxrss,
		}
	}
	return nil
}
