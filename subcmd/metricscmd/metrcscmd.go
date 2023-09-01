// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package metricscmd provides metrics subcommand.
package metricscmd

import (
	"os"

	"github.com/maruel/subcommands"
)

// Cmd returns the Command for the `metrics` subcommand provided by this package.
func Cmd() *subcommands.Command {
	return &subcommands.Command{
		UsageLine: "metrics <subcommand>",
		ShortDesc: "analyze siso_metrics.json",
		LongDesc:  "analyze siso_metrics.json",
		Advanced:  true,
		CommandRun: func() subcommands.CommandRun {
			c := &metricsRun{
				app: &subcommands.DefaultApplication{
					Name:  "siso metrics",
					Title: "tools to analyze siso_metrics.json",
					Commands: []*subcommands.Command{
						cmpCmd(),
						summaryCmd(),
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

type metricsRun struct {
	subcommands.CommandRunBase
	app *subcommands.DefaultApplication
}

func (c *metricsRun) Run(a subcommands.Application, args []string, env subcommands.Env) int {
	return subcommands.Run(c.app, args)
}
