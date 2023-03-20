// Copyright 2023 The Chromium Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package ninja implements the subcommand `ninja` which parses a `build.ninja` file and builds the requested targets.
package ninja

import (
	"fmt"
	"os"

	"infra/build/siso/auth/cred"

	"github.com/maruel/subcommands"
	"go.chromium.org/luci/auth"
	"go.chromium.org/luci/common/cli"
)

// Cmd returns the Command for the `ninja` subcommand provided by this package.
func Cmd(authOpts auth.Options) *subcommands.Command {
	return &subcommands.Command{
		UsageLine: "ninja <args>...",
		CommandRun: func() subcommands.CommandRun {
			r := ninjaCmdRun{
				authOpts: authOpts,
			}
			return &r
		},
	}
}

type ninjaCmdRun struct {
	subcommands.CommandRunBase
	authOpts auth.Options
}

// Run runs the `ninja` subcommand.
func (c *ninjaCmdRun) Run(a subcommands.Application, args []string, env subcommands.Env) int {
	ctx := cli.GetContext(a, c, env)

	_, err := cred.New(ctx, c.authOpts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Failed to authenticate. Please login with `siso login`.")
		return 1
	}

	// Use au.PerRPCCredentials() to get PerRPCCredentials of google.golang.org/grpc/credentials.
	// Use au.TokenSource() to get oauth2.TokenSource.
	return 0
}
