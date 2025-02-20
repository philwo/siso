// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package query

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/maruel/subcommands"

	"go.chromium.org/luci/common/cli"

	"go.chromium.org/infra/build/siso/toolsupport/ninjautil"
)

const ruleUsage = `query build rule

 $ siso query rule -C <dir> <targets>

prints filtered ninja build rules for <targets>.
`

// cmdRule returns the Command for the `rule` subcommand provided by this package.
func cmdRule() *subcommands.Command {
	return &subcommands.Command{
		UsageLine: "rule [-C <dir>] [<targets>...]",
		ShortDesc: "query build step rule",
		LongDesc:  ruleUsage,
		CommandRun: func() subcommands.CommandRun {
			c := &ruleRun{}
			c.init()
			return c
		},
	}
}

type ruleRun struct {
	subcommands.CommandRunBase

	dir     string
	fname   string
	binding string
}

func (c *ruleRun) init() {
	c.Flags.StringVar(&c.dir, "C", ".", "ninja running directory to find build.ninja")
	c.Flags.StringVar(&c.fname, "f", "build.ninja", "input build filename (relative to -C")
	c.Flags.StringVar(&c.binding, "binding", "", "print binding value for the target")
}

func (c *ruleRun) Run(a subcommands.Application, args []string, env subcommands.Env) int {
	ctx := cli.GetContext(a, c, env)
	err := c.run(ctx, args)
	if err != nil {
		switch {
		case errors.Is(err, flag.ErrHelp):
			fmt.Fprintf(os.Stderr, "%v\n%s\n", err, ruleUsage)
		default:
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		return 1
	}
	return 0
}

func (c *ruleRun) run(ctx context.Context, args []string) error {
	state := ninjautil.NewState()
	p := ninjautil.NewManifestParser(state)
	err := os.Chdir(c.dir)
	if err != nil {
		return err
	}
	err = p.Load(ctx, c.fname)
	if err != nil {
		return err
	}
	nodes, err := state.Targets(args)
	if err != nil {
		return err
	}
	for _, node := range nodes {
		edge, ok := node.InEdge()
		if !ok {
			fmt.Printf("# no rule to build %s\n\n", node.Path())
			continue
		}
		if c.binding != "" {
			fmt.Println(edge.Binding(c.binding))
			continue
		}
		edge.Print(os.Stdout)
		fmt.Printf("# %s is used by the following targets\n", node.Path())
		for _, edge := range node.OutEdges() {
			outs := edge.Outputs()
			if len(outs) == 0 {
				continue
			}
			fmt.Printf("#  %s\n", outs[0].Path())
		}
		fmt.Printf("\n\n")
	}
	return nil
}
