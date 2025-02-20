// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package fscmd provides fs subcommand.
package fscmd

import (
	"os"

	"github.com/maruel/subcommands"

	"go.chromium.org/infra/build/siso/auth/cred"
)

const stateFile = ".siso_fs_state"

// Cmd returns the Command for the `fs` subcommand provided by this package.
func Cmd(authOpts cred.Options) *subcommands.Command {
	return &subcommands.Command{
		UsageLine: "fs <subcommand>",
		ShortDesc: "access siso hashfs data",
		LongDesc:  "access siso hashfs data.",
		Advanced:  true,
		CommandRun: func() subcommands.CommandRun {
			c := &fsRun{
				app: &subcommands.DefaultApplication{
					Name:  "siso fs",
					Title: "tool to access siso hashfs data",
					Commands: []*subcommands.Command{
						cmdFSDiff(),
						cmdFSExport(),
						cmdFSFlush(authOpts),
						cmdFSImport(),
						subcommands.CmdHelp,
					},
				},
			}
			c.Flags.Usage = func() {
				// TODO: handle -advanced?
				subcommands.Usage(os.Stderr, c.app, true)
			}
			return c
		},
	}
}

type fsRun struct {
	subcommands.CommandRunBase
	app *subcommands.DefaultApplication
}

func (c *fsRun) Run(a subcommands.Application, args []string, env subcommands.Env) int {
	return subcommands.Run(c.app, args)
}
