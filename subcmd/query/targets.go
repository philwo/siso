// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package query

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/maruel/subcommands"

	"go.chromium.org/luci/common/cli"

	"go.chromium.org/infra/build/siso/toolsupport/ninjautil"
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
			c := &targetsRun{w: os.Stdout}
			c.init()
			return c
		},
	}
}

type targetsRun struct {
	subcommands.CommandRunBase
	w io.Writer

	dir   string
	fname string

	rule  targetRuleFlag
	depth int
	all   bool
}

type targetRuleFlag struct {
	rule      string
	requested bool
}

func (f *targetRuleFlag) String() string {
	return f.rule
}

func (f *targetRuleFlag) Set(v string) error {
	f.rule = v
	f.requested = true
	return nil
}

func (c *targetsRun) init() {
	// TODO(b/340381100): extract common flags for ninja commands.
	c.Flags.StringVar(&c.dir, "C", ".", "ninja running directory to find build.ninja")
	c.Flags.StringVar(&c.fname, "f", "build.ninja", "input build filename (relative to -C)")

	c.Flags.Var(&c.rule, "rule", "rule name for the targets")
	c.Flags.IntVar(&c.depth, "depth", 1, "max depth of the targets. 0 does not check depth")
	c.Flags.BoolVar(&c.all, "all", false, "list all targets")
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
	if c.rule.requested {
		c.depth = 0
	}
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
		w:           c.w,
		all:         c.all,
		rule:        &c.rule,
		ruleTargets: make(map[string]bool),
	}
	for _, n := range nodes {
		err := g.Traverse(ctx, state, n, c.depth, 0)
		if err != nil {
			return err
		}
	}
	if c.rule.requested {
		var targets []string
		for t := range g.ruleTargets {
			targets = append(targets, t)
		}
		sort.Strings(targets)
		for _, t := range targets {
			fmt.Fprintln(c.w, t)
		}
	}
	return nil
}

type targetsGraph struct {
	seen        map[*ninjautil.Node]bool
	w           io.Writer
	all         bool
	rule        *targetRuleFlag
	ruleTargets map[string]bool
}

func (g *targetsGraph) Traverse(ctx context.Context, state *ninjautil.State, node *ninjautil.Node, depth, indent int) error {
	if g.seen[node] {
		return nil
	}
	g.seen[node] = true
	prefix := strings.Repeat(" ", indent)
	edge, ok := node.InEdge()
	if !ok {
		if g.rule.requested && g.rule.rule == "" {
			g.ruleTargets[node.Path()] = true
		}
		return nil
	}
	switch {
	case g.rule.requested:
		if g.rule.rule == edge.RuleName() {
			g.ruleTargets[node.Path()] = true
		}
	case g.all:
		fmt.Fprintf(g.w, "%s: %s\n", node.Path(), edge.RuleName())
	default:
		fmt.Fprintf(g.w, "%s%s: %s\n", prefix, node.Path(), edge.RuleName())
	}
	if !g.all && depth == 1 {
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
