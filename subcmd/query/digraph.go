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
	"strings"

	"github.com/maruel/subcommands"

	"go.chromium.org/luci/common/cli"

	"infra/build/siso/toolsupport/ninjautil"
)

const digraphUsage = `show digraph

 $ siso query digraph -C <dir> <targets>

prints directed graph for <targets> of build.ninja.
If <targets> is not give, it will print directed graph for default target specified by build.ninja.
Each line contains zero or more targets, and the first target depends on
the rest of the targets on the same line.

This output can be passed to digraph command, installed by
 $ go install golang.org/x/tools/cmd/digraph@latest

See https://pkg.go.dev/golang.org/x/tools/cmd/digraph
for digraph command.
`

// cmdDigraph returns the Command for the `digraph` subcommand provided by this package.
func cmdDigraph() *subcommands.Command {
	return &subcommands.Command{
		UsageLine: "digraph [-C <dir>] [<targets>...]",
		ShortDesc: "show digraph",
		LongDesc:  digraphUsage,
		Advanced:  true,
		CommandRun: func() subcommands.CommandRun {
			c := &digraphRun{}
			c.init()
			return c
		},
	}
}

type digraphRun struct {
	subcommands.CommandRunBase

	dir   string
	fname string
}

func (c *digraphRun) init() {
	c.Flags.StringVar(&c.dir, "C", ".", "ninja running directory to find build.ninja")
	c.Flags.StringVar(&c.fname, "f", "build.ninja", "input build filename (relative to -C)")
}

func (c *digraphRun) Run(a subcommands.Application, args []string, env subcommands.Env) int {
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

func (c *digraphRun) run(ctx context.Context, args []string) error {
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
	d := &digraph{
		seen: make(map[string]bool),
	}
	for _, t := range targets {
		err := d.Traverse(ctx, state, t)
		if err != nil {
			return err
		}
	}
	return nil
}

type digraph struct {
	seen map[string]bool
}

func (d *digraph) Traverse(ctx context.Context, state *ninjautil.State, target string) error {
	if d.seen[target] {
		return nil
	}
	d.seen[target] = true
	n, ok := state.LookupNode(target)
	if !ok {
		return fmt.Errorf("target not found: %q", target)
	}
	edge, ok := n.InEdge()
	if !ok {
		fmt.Printf("%s\n", target)
		return nil
	}
	var inputs []string
	for _, in := range edge.Inputs() {
		p := in.Path()
		err := d.Traverse(ctx, state, p)
		if err != nil {
			return err
		}
		inputs = append(inputs, p)
	}
	fmt.Printf("%s %s\n", target, strings.Join(inputs, " "))
	return nil
}
