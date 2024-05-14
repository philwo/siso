// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build unix

package osfs

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"sync"
)

var executableOnce = sync.OnceValues(os.Executable)

func writeFile(name string, data []byte, perm os.FileMode) error {
	if perm&0100 == 0 {
		return os.WriteFile(name, data, perm)
	}
	// workaround for https://github.com/golang/go/issues/22315
	//
	// ETXTBSY race when creating executable and running the executable
	// from the same process, because when starting a command (not
	// running the executable), fork inherits file descriptor of creating
	// executable, which will be closed when exec by O_CLOEXEC, but
	// ETXTBSY if running executable starts earlier before the exec.
	//
	// Goma used subprocess server, and gomax used localexec-server,
	// so that fork&exec from the process that didn't create executables.
	// but it spawns new sub process and it involves some complexity
	// (i.e. we'll use unix domain socket to communicate, but we can't
	// create unix domain socket in Cog workspace)
	//
	// It will run a new process to create executable file, so that
	// any build steps won't inherit file descriptor to write executable
	// file.  The file descriptor should have been closed when this process
	// finished.
	executable, err := executableOnce()
	if err != nil {
		return err
	}
	cmd := exec.Command(executable, "install-helper", "-m", fmt.Sprintf("0%o", perm), "-o", name)
	cmd.Stdin = bytes.NewReader(data)
	return cmd.Run()
}
