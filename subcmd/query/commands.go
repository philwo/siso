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

	"infra/build/siso/toolsupport/ninjautil"
)

const commandsUsage = `list all commands required to rebuild given targets

 $ siso query commands -C <dir> <targets>

prints all commands required to rebuild given targets.
`

// cmdCommands returns the Command for the `commands` subcommand provided by this package.
func cmdCommands() *subcommands.Command {
	return &subcommands.Command{
		UsageLine: "commands [-C <dir>] [<targets>...]",
		ShortDesc: "list all commands required to rebuild given targets",
		LongDesc:  commandsUsage,
		CommandRun: func() subcommands.CommandRun {
			c := &commandsRun{}
			c.init()
			return c
		},
	}
}

type commandsRun struct {
	subcommands.CommandRunBase

	dir   string
	fname string
}

func (c *commandsRun) init() {
	c.Flags.StringVar(&c.dir, "C", ".", "ninja running directory to find build.ninja")
	c.Flags.StringVar(&c.fname, "f", "build.ninja", "input build filename (relative to -C)")
}

func (c *commandsRun) Run(a subcommands.Application, args []string, env subcommands.Env) int {
	ctx := cli.GetContext(a, c, env)
	err := c.run(ctx, args)
	if err != nil {
		switch {
		case errors.Is(err, flag.ErrHelp):
			fmt.Fprintf(os.Stderr, "%v\n%s\n", err, digraphUsage)
		default:
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		return 1
	}
	return 0
}

func (c *commandsRun) run(ctx context.Context, args []string) error {
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
	targets := make([]string, 0, len(nodes))
	for _, n := range nodes {
		targets = append(targets, n.Path())
	}
	g := &commandsGraph{
		seen: make(map[string]bool),
	}
	for _, t := range targets {
		err := g.Traverse(ctx, state, t)
		if err != nil {
			return err
		}
	}
	return nil
}

type commandsGraph struct {
	seen map[string]bool
}

func (g *commandsGraph) Traverse(ctx context.Context, state *ninjautil.State, target string) error {
	if g.seen[target] {
		return nil
	}
	g.seen[target] = true
	n, ok := state.LookupNode(target)
	if !ok {
		return fmt.Errorf("target not found: %q", target)
	}
	edge, ok := n.InEdge()
	if !ok {
		return nil
	}
	for _, in := range edge.Inputs() {
		p := in.Path()
		err := g.Traverse(ctx, state, p)
		if err != nil {
			return err
		}
	}
	fmt.Printf("%s\n", edge.Binding("command"))
	return nil
}
