// Copyright 2023 The Chromium Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime"

	log "github.com/golang/glog"
	"go.chromium.org/luci/common/system/signals"
)

// Siso is an experimental build tool.

func main() {
	flag.Usage = func() {
		out := flag.CommandLine.Output()
		fmt.Fprintf(out, "Usage of %s:\n", os.Args[0])
		fmt.Fprintf(out, "global flags:\n")
		flag.PrintDefaults()
	}

	flag.Parse()
	if flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}
	ctx := context.Background()
	var err error

	err = sisoMain(ctx, flag.Args())

	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		flag.Usage()
		os.Exit(2)
	}
	if err != nil {
		log.Exitf("Error: %v", err)
	}
}

func sisoMain(ctx context.Context, args []string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer signals.HandleInterrupt(cancel)()

	// Flush the log on exit to not lose any messages.
	defer log.Flush()

	// Print a stack trace when a panic occurs.
	defer func() {
		if r := recover(); r != nil {
			const size = 64 << 10
			buf := make([]byte, size)
			buf = buf[:runtime.Stack(buf, false)]
			log.Fatalf("panic: %v\n%s", r, buf)
		}
	}()

	var err error
	// TODO(b/246687010) use subcommands library
	switch args[0] {
	default:
		err = fmt.Errorf("unknown subcommand %q: %w", args[0], flag.ErrHelp)
	}
	return err
}
