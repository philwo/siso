// Copyright 2023 The Chromium Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package main

import (
	"context"
	"flag"
	"os"
	"runtime"

	log "github.com/golang/glog"
	"github.com/maruel/subcommands"
	"go.chromium.org/luci/auth"
	"go.chromium.org/luci/auth/client/authcli"
	"go.chromium.org/luci/client/versioncli"
	"go.chromium.org/luci/common/cli"
	"go.chromium.org/luci/common/system/signals"
	"go.chromium.org/luci/hardcoded/chromeinfra"

	"infra/build/siso/subcmd/ninja"
)

// Siso is an experimental build tool.

const version = "0.1"

func getApplication() *cli.Application {
	// Use go.chromium.org/luci/auth to authenticate.
	authOpts := chromeinfra.DefaultAuthOptions()
	authOpts.Scopes = []string{auth.OAuthScopeEmail, "https://www.googleapis.com/auth/cloud-platform"}

	return &cli.Application{
		Name:  "siso",
		Title: "Build system with Remote Build Execution",
		Context: func(ctx context.Context) context.Context {
			ctx, cancel := context.WithCancel(ctx)
			signals.HandleInterrupt(cancel)()
			return ctx
		},
		Commands: []*subcommands.Command{
			subcommands.CmdHelp,

			ninja.Cmd(authOpts),

			authcli.SubcommandInfo(authOpts, "whoami", false),
			authcli.SubcommandLogin(authOpts, "login", false),
			authcli.SubcommandLogout(authOpts, "logout", false),
			versioncli.CmdVersion(version),
		},
	}
}

func main() {
	flag.Parse()
	// Wraps sisoMain() because os.Exit() doesn't wait defers.
	os.Exit(sisoMain())
}

func sisoMain() int {

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

	return subcommands.Run(getApplication(), nil)
}
