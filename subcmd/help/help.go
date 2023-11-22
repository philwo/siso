// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package help provides version subcommand.
package help

import (
	"flag"
	"fmt"

	"github.com/maruel/subcommands"
)

// Cmd returns the Command for the `help` subcommand provided by this package.
func Cmd() *subcommands.Command {
	return &subcommands.Command{
		UsageLine: "help [<command>|-advanced]",
		ShortDesc: "prints help about a command",
		LongDesc:  "Prints commands and globally-available flags or help about a specific command.\nUse -advanced to display all commands.",
		CommandRun: func() subcommands.CommandRun {
			ret := &helpCmdRun{}
			ret.Flags.BoolVar(&ret.advanced, "advanced", false, "show advanced commands")
			return ret
		},
	}
}

type helpCmdRun struct {
	subcommands.CommandRunBase
	advanced bool
}

func (h *helpCmdRun) Run(a subcommands.Application, args []string, env subcommands.Env) int {
	// For top-level help, print subcommands.Usage. Then print flags.
	if len(args) == 0 {
		subcommands.Usage(a.GetOut(), a, h.advanced)
		fmt.Println("Common flags accepted by all commands:")
		flag.PrintDefaults()
		return 0
	}

	// Use default subcommands.CmdHelp for all other cases.
	helpInit := subcommands.CmdHelp.CommandRun()
	result := helpInit.Run(a, args, env)
	return result
}
