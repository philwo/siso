// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package webui provides webui subcommand.
package webui

import (
	"github.com/maruel/subcommands"

	"infra/build/siso/webui"
)

func Cmd() *subcommands.Command {
	return &subcommands.Command{
		UsageLine: "webui <args>",
		Advanced:  true,
		ShortDesc: "starts the experimental webui",
		LongDesc:  "Starts the experimental webui. Not ready for wide use yet, requires static files to work. This is subject to breaking changes at any moment.",
		CommandRun: func() subcommands.CommandRun {
			r := &webuiRun{}
			r.init()
			return r
		},
	}
}

type webuiRun struct {
	subcommands.CommandRunBase
	port        int
	metricsJSON string
}

func (c *webuiRun) init() {
	c.Flags.IntVar(&c.port, "port", 8080, "port to use (defaults to 8080)")
	c.Flags.StringVar(&c.metricsJSON, "metrics", "", "path to siso_metrics.json")
}

func (c *webuiRun) Run(a subcommands.Application, args []string, env subcommands.Env) int {
	return webui.Serve(c.port, c.metricsJSON)
}
