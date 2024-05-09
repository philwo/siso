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
	"sort"

	"github.com/maruel/subcommands"

	"go.chromium.org/luci/common/cli"

	"infra/build/siso/toolsupport/ninjautil"
)

const inputsUsage = `list all inputs required to rebuild given targets

 $ siso query inputs -C <dir> <targets>

prints all inputs required to rebuild given targets.
`

// cmdInputs returns the Command for the `inputs` subcommand provided by this package.
func cmdInputs() *subcommands.Command {
	return &subcommands.Command{
		UsageLine: "inputs [-C <dir>] [<targets>...]",
		ShortDesc: "list all inputs required to rebuild given targets",
		LongDesc:  inputsUsage,
		CommandRun: func() subcommands.CommandRun {
			c := &inputsRun{}
			c.init()
			return c
		},
	}
}

type inputsRun struct {
	subcommands.CommandRunBase

	dir   string
	fname string
}

func (c *inputsRun) init() {
	c.Flags.StringVar(&c.dir, "C", ".", "ninja running directory to find build.ninja")
	c.Flags.StringVar(&c.fname, "f", "build.ninja", "input build filename (relative to -C)")
}

func (c *inputsRun) Run(a subcommands.Application, args []string, env subcommands.Env) int {
	ctx := cli.GetContext(a, c, env)
	err := c.run(ctx, args)
	if err != nil {
		switch {
		case errors.Is(err, flag.ErrHelp):
			fmt.Fprintf(os.Stderr, "%v\n%s\n", err, inputsUsage)
		default:
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		return 1
	}
	return 0
}

func (c *inputsRun) run(ctx context.Context, args []string) error {
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
	g := &inputsGraph{
		seen: make(map[string]bool),
	}
	isTargets := make(map[string]bool)
	for _, t := range targets {
		isTargets[t] = true
		err := g.Traverse(ctx, state, t)
		if err != nil {
			return err
		}
	}
	var inputs []string
	for t := range g.seen {
		if isTargets[t] {
			continue
		}
		inputs = append(inputs, t)
	}
	sort.Strings(inputs)
	for _, in := range inputs {
		fmt.Println(in)
	}
	return nil
}

type inputsGraph struct {
	seen map[string]bool
}

func (g *inputsGraph) Traverse(ctx context.Context, state *ninjautil.State, target string) error {
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
	return nil
}
