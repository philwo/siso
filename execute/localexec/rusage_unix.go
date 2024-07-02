// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build unix

package localexec

import (
	"os/exec"
	"syscall"

	durationpb "google.golang.org/protobuf/types/known/durationpb"

	epb "infra/build/siso/execute/proto"
)

func rusage(cmd *exec.Cmd) *epb.Rusage {
	if u, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage); ok {
		return &epb.Rusage{
			// 32bit arch may use int32 for Maxrss etc.
			MaxRss:  int64(u.Maxrss),
			Majflt:  int64(u.Majflt),
			Inblock: int64(u.Inblock),
			Oublock: int64(u.Oublock),
			Utime:   &durationpb.Duration{Seconds: u.Utime.Sec, Nanos: int32(u.Utime.Usec)},
			Stime:   &durationpb.Duration{Seconds: u.Stime.Sec, Nanos: int32(u.Stime.Usec)},
		}
	}
	return nil
}
