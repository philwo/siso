// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package webui provides webui subcommand.
package webui

import (
	"fmt"
	"os"

	"github.com/maruel/subcommands"

	"infra/build/siso/webui"
)

func Cmd(version string) *subcommands.Command {
	return &subcommands.Command{
		UsageLine: "webui <args>",
		Advanced:  true,
		ShortDesc: "starts the experimental webui",
		LongDesc:  "Starts the experimental webui. Not ready for wide use yet, requires static files to work. This is subject to breaking changes at any moment.",
		CommandRun: func() subcommands.CommandRun {
			r := &webuiRun{
				version: version,
			}
			r.init()
			return r
		},
	}
}

type webuiRun struct {
	subcommands.CommandRunBase
	version          string
	localDevelopment bool
	port             int
	outdir           string
	configRepoDir    string
}

func (c *webuiRun) init() {
	c.Flags.BoolVar(&c.localDevelopment, "local_development", false, "whether to use local instead of embedded files")
	c.Flags.IntVar(&c.port, "port", 8080, "port to use (defaults to 8080)")
	c.Flags.StringVar(&c.outdir, "C", "", "path to outdir")
	c.Flags.StringVar(&c.configRepoDir, "config_repo_dir", "build/config/siso", "config repo directory (relative to exec root)")
}

func (c *webuiRun) Run(a subcommands.Application, args []string, env subcommands.Env) int {
	s, err := webui.NewServer(c.version, c.localDevelopment, c.port, c.outdir, c.configRepoDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to init server: %v\n", err)
		return 1
	}
	return s.Serve()
}
