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
	"strings"

	"github.com/maruel/subcommands"

	"go.chromium.org/luci/common/cli"

	"infra/build/siso/toolsupport/ninjautil"
)

const targetsUsage = `list targets by their rule or depth in the DAG

 $ siso query targets -C <dir> [--rule <rule>] [--depth <depth>]

prints targets by <rule> or in <depth>.
`

// cmdTargets returns the Command for the `targets` subcommand provided by this package.
func cmdTargets() *subcommands.Command {
	return &subcommands.Command{
		UsageLine: "targets [-C <dir>] [--rule <rule>] [--depth <depth>]",
		ShortDesc: "list targets by their rule or depth in the DAG",
		LongDesc:  targetsUsage,
		CommandRun: func() subcommands.CommandRun {
			c := &targetsRun{}
			c.init()
			return c
		},
	}
}

type targetsRun struct {
	subcommands.CommandRunBase

	dir   string
	fname string

	ruleName string
	depth    int
}

func (c *targetsRun) init() {
	// TODO(b/340381100): extract common flags for ninja commands.
	c.Flags.StringVar(&c.dir, "C", ".", "ninja running directory to find build.ninja")
	c.Flags.StringVar(&c.fname, "f", "build.ninja", "input build filename (relative to -C)")

	c.Flags.StringVar(&c.ruleName, "rule", "", "rule name for the targets")
	c.Flags.IntVar(&c.depth, "depth", 0, "max depth of the targets. 0 does not check depth")
}

func (c *targetsRun) Run(a subcommands.Application, args []string, env subcommands.Env) int {
	ctx := cli.GetContext(a, c, env)
	err := c.run(ctx, args)
	if err != nil {
		switch {
		case errors.Is(err, flag.ErrHelp):
			fmt.Fprintf(os.Stderr, "%v\n%s\n", err, targetsUsage)
		default:
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		return 1
	}
	return 0
}

func (c *targetsRun) run(ctx context.Context, args []string) error {
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
	nodes, err := state.RootNodes()
	if err != nil {
		return err
	}
	g := &targetsGraph{
		seen:        make(map[*ninjautil.Node]bool),
		ruleName:    c.ruleName,
		ruleTargets: make(map[string]bool),
	}
	for _, n := range nodes {
		err := g.Traverse(ctx, state, n, c.depth, 0)
		if err != nil {
			return err
		}
	}
	if c.ruleName != "" {
		var targets []string
		for t := range g.ruleTargets {
			targets = append(targets, t)
		}
		sort.Strings(targets)
		for _, t := range targets {
			fmt.Println(t)
		}
	}
	return nil
}

type targetsGraph struct {
	seen        map[*ninjautil.Node]bool
	ruleName    string
	ruleTargets map[string]bool
}

func (g *targetsGraph) Traverse(ctx context.Context, state *ninjautil.State, node *ninjautil.Node, depth, indent int) error {
	if g.seen[node] {
		return nil
	}
	g.seen[node] = true
	var prefix string
	if depth > 0 {
		prefix = strings.Repeat(" ", indent)
	}
	edge, ok := node.InEdge()
	if !ok {
		return nil
	}
	if g.ruleName != "" {
		if g.ruleName == edge.RuleName() {
			g.ruleTargets[node.Path()] = true
		}
	} else {
		fmt.Printf("%s%s: %s\n", prefix, node.Path(), edge.RuleName())
	}
	if depth == 1 {
		return nil
	}
	for _, in := range edge.Inputs() {
		err := g.Traverse(ctx, state, in, depth-1, indent+1)
		if err != nil {
			return err
		}
	}
	return nil
}
