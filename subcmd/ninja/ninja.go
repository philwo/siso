// Copyright 2023 The Chromium Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"fmt"
	"os"

	"github.com/maruel/subcommands"
	"go.chromium.org/luci/auth"
	"go.chromium.org/luci/common/cli"
)

func Cmd(authOpts auth.Options) *subcommands.Command {
	return &subcommands.Command{
		UsageLine: "ninja <args>...",
		CommandRun: func() subcommands.CommandRun {
			r := ninjaCmdRun{authOpts: authOpts}
			return &r
		},
	}
}

type ninjaCmdRun struct {
	subcommands.CommandRunBase
	authOpts auth.Options
}

func (c *ninjaCmdRun) Run(a subcommands.Application, args []string, env subcommands.Env) int {
	ctx := cli.GetContext(a, c, env)

	au := auth.NewAuthenticator(ctx, auth.SilentLogin, c.authOpts)
	if err := au.CheckLoginRequired(); err != nil {
		fmt.Fprintln(os.Stderr, "Failed to authenticate. Please login with `siso login`")
		return 1
	}
	// Use au.PerRPCCredentials() to get PerRPCCredentials of google.golang.org/grpc/credentials.
	// Use au.TokenSource() to get oauth2.TokenSource.
	return 0
}
