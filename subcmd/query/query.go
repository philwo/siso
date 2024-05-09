// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package query is ninja_query subcommand to query ninja build graph.
package query

import (
	"os"

	"github.com/maruel/subcommands"
)

func Cmd() *subcommands.Command {
	return &subcommands.Command{
		UsageLine: "query [-C <dir>] ...",
		ShortDesc: "query ninja build graph",
		LongDesc:  "query ninja build graph.",
		CommandRun: func() subcommands.CommandRun {
			c := &run{
				app: &subcommands.DefaultApplication{
					Name:  "siso query",
					Title: "tool to access ninja build graph",
					Commands: []*subcommands.Command{
						cmdCommands(),
						cmdDigraph(),
						cmdIDEAnalysis(),
						cmdInputs(),
						cmdRule(),
						// TODO: add more subcommands (ninja's tool like commands, etc.
						subcommands.CmdHelp,
					},
				},
			}
			c.Flags.Usage = func() {
				subcommands.Usage(os.Stderr, c.app, true)
			}
			return c
		},
	}
}

type run struct {
	subcommands.CommandRunBase
	app *subcommands.DefaultApplication
}

func (c *run) Run(a subcommands.Application, args []string, env subcommands.Env) int {
	return subcommands.Run(c.app, args)
}
