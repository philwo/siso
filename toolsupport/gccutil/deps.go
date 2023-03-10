// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package gccutil provides utilities of gcc.
package gccutil

import (
	"context"
	"runtime"
	"strings"
	"time"

	"infra/build/siso/execute"
	"infra/build/siso/execute/localexec"
	"infra/build/siso/o11y/clog"
	"infra/build/siso/sync/semaphore"
	"infra/build/siso/toolsupport/makeutil"
)

var Semaphore = semaphore.New("deps-gcc", runtime.NumCPU()*2)

// DepsArgs returns command line args to get deps for args.
func DepsArgs(args []string) []string {
	var dargs []string
	skip := false
	for _, arg := range args {
		if skip {
			skip = false
			continue
		}
		switch arg {
		case "-MD", "-MMD", "-c":
			continue
		case "-MF", "-o":
			skip = true
			continue
		}
		if strings.HasPrefix(arg, "-MF") {
			continue
		}
		if strings.HasPrefix(arg, "-o") {
			continue
		}
		dargs = append(dargs, arg)
	}
	dargs = append(dargs, "-M")
	return dargs
}

// Deps runs command specified by args, env, cwd and returns deps.
func Deps(ctx context.Context, args []string, env []string, cwd string) ([]string, error) {
	s := time.Now()
	cmd := &execute.Cmd{
		Args:     args,
		Env:      env,
		ExecRoot: cwd,
	}
	var wait time.Duration
	err := Semaphore.Do(ctx, func(ctx context.Context) error {
		wait = time.Since(s)
		return localexec.Run(ctx, cmd)
	})
	if err != nil {
		clog.Warningf(ctx, "failed to run %q: %v\n%s\n%s", args, err, cmd.Stdout(), cmd.Stderr())
		return nil, err
	}
	stdout := cmd.Stdout()
	if len(stdout) == 0 {
		clog.Warningf(ctx, "failed to run gcc deps? stdout:0 args:%q\nstderr:%s", cmd.Args, cmd.Stderr())
	}
	deps := makeutil.ParseDeps(stdout)
	clog.Infof(ctx, "gcc deps stdout:%d -> deps:%d: %s (wait:%s)", len(stdout), len(deps), time.Since(s), wait)
	return deps, nil
}
