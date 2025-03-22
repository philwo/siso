// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build unix

package ninja

import (
	"context"
	"fmt"

	"github.com/charmbracelet/log"
	"golang.org/x/sys/unix"

	"go.chromium.org/infra/build/siso/build"
	"go.chromium.org/infra/build/siso/ui"
)

func (c *ninjaCmdRun) checkResourceLimits(ctx context.Context) {
	var lim unix.Rlimit
	err := unix.Getrlimit(unix.RLIMIT_NOFILE, &lim)
	if err != nil {
		log.Warnf("failed to get rlimit: %v", err)
		return
	}
	limits := build.DefaultLimits(ctx)
	nfile := uint64(limits.Local) * 8 // 8 fds per proc?
	switch {
	case c.offline:
	case c.remoteJobs > 0:
		// reproxy grpc client+server, scandeps server client+server
		nfile += uint64(c.remoteJobs) * 4
	default:
		nfile += uint64(limits.Remote) * 4
	}
	log.Infof("rlimit.nofile=%d,%d required=%d?", lim.Cur, lim.Max, nfile)
	if lim.Cur < nfile {
		ui.Default.PrintLines(ui.SGR(ui.Yellow, fmt.Sprintf("WARNING: too low file limit=%d. would fail with too many open files\n", lim.Cur)))
	}
}
