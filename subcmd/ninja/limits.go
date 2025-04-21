// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"github.com/charmbracelet/log"
	"golang.org/x/sys/unix"
)

func (c *NinjaOpts) checkResourceLimits() {
	var lim unix.Rlimit
	err := unix.Getrlimit(unix.RLIMIT_NOFILE, &lim)
	if err != nil {
		log.Warnf("failed to get rlimit: %v", err)
		return
	}
	nfile := uint64(limits.Local) * 8 // 8 fds per proc?
	switch {
	case c.Offline:
	case c.RemoteJobs > 0:
		// scandeps server client+server
		nfile += uint64(c.RemoteJobs) * 4
	default:
		nfile += uint64(limits.Remote) * 4
	}
	if lim.Cur < nfile {
		log.Infof("WARNING: too low file limit=%d. would fail with too many open files", lim.Cur)
	}
}
