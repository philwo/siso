// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package query

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/maruel/subcommands"

	"go.chromium.org/luci/common/cli"

	"go.chromium.org/infra/build/siso/hashfs"
	"go.chromium.org/infra/build/siso/o11y/clog"
	"go.chromium.org/infra/build/siso/toolsupport/makeutil"
	"go.chromium.org/infra/build/siso/toolsupport/ninjautil"
)

const depsUsage = `show dependencies stored in the deps log or depfile

 $ siso query deps -C <dir> [<targets>]

print dependencies for targets stored in the deps log.

----
<target>: #deps <num> deps mtime <mtime> ([STALE|VALID])
  <deps>
  ...

----

or depfile
----
<target>: #depfile=<depfile> <num> deps mtime <mtime> VALID
  <deps>
  ...

----

`

// cmdDeps returns the Command for the `deps` subcommand provided by this package.
func cmdDeps() *subcommands.Command {
	return &subcommands.Command{
		UsageLine: "deps [-C <dir>] [<targets>...]",
		ShortDesc: "show dependencies stored in the deps log",
		LongDesc:  depsUsage,
		CommandRun: func() subcommands.CommandRun {
			c := &depsRun{}
			c.init()
			return c
		},
	}
}

type depsRun struct {
	subcommands.CommandRunBase

	dir         string
	fname       string
	fsopt       *hashfs.Option
	depsLogFile string
	raw         bool
	depfile     bool
}

func (c *depsRun) init() {
	c.Flags.StringVar(&c.dir, "C", ".", "ninja running directory to find dpes log")
	c.Flags.StringVar(&c.fname, "f", "build.ninja", "input build filename (relative to -C)")
	c.fsopt = new(hashfs.Option)
	c.fsopt.StateFile = ".siso_fs_state"
	c.fsopt.RegisterFlags(&c.Flags)
	c.Flags.StringVar(&c.depsLogFile, "deps_log", ".siso_deps", "deps log filename (relative to -C)")
	c.Flags.BoolVar(&c.raw, "raw", false, "just check deps log. (no build.ninja nor .siso_fs_state needed)")
	c.Flags.BoolVar(&c.depfile, "depfile", false, "check depfile too")
}

func (c *depsRun) Run(a subcommands.Application, args []string, env subcommands.Env) int {
	ctx := cli.GetContext(a, c, env)
	err := c.run(ctx, args)
	if err != nil {
		switch {
		case errors.Is(err, flag.ErrHelp):
			fmt.Fprintf(os.Stderr, "%v\n%s\n", err, depsUsage)
		default:
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		return 1
	}
	return 0
}

func (c *depsRun) run(ctx context.Context, args []string) error {
	err := os.Chdir(c.dir)
	if err != nil {
		return err
	}
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	wd, err = filepath.EvalSymlinks(wd)
	if err != nil {
		return err
	}

	depsLog, err := ninjautil.NewDepsLog(ctx, c.depsLogFile)
	if err != nil {
		return err
	}

	var hashFS *hashfs.HashFS
	var state *ninjautil.State
	targets := args
	if c.raw {
		if len(targets) == 0 {
			targets = depsLog.RecordedTargets()
		}
	} else {
		var err error
		hashFS, err = hashfs.New(ctx, hashfs.Option{})
		if err != nil {
			return err
		}
		fsstate, err := hashfs.Load(ctx, hashfs.Option{StateFile: c.fsopt.StateFile})
		if err != nil {
			return err
		}
		err = hashFS.SetState(ctx, fsstate)
		if err != nil {
			return err
		}

		state = ninjautil.NewState()
		p := ninjautil.NewManifestParser(state)
		err = p.Load(ctx, c.fname)
		if err != nil {
			return err
		}
		targets, err = depsTargets(ctx, state, depsLog, args)
		if err != nil {
			return err
		}
	}
	if !c.depfile {
		state = nil
	}
	w := bufio.NewWriter(os.Stdout)
	for _, target := range targets {
		depType, deps, depsTime, depState, err := lookupDeps(ctx, state, hashFS, depsLog, wd, target)
		if err != nil {
			if errors.Is(err, ninjautil.ErrNoDepsLog) {
				continue
			}
			fmt.Fprintf(w, "%s: deps log error: %v\n", target, err)
			continue
		}
		var buf bytes.Buffer
		fmt.Fprintf(&buf, "%s: #%s %d, deps mtime %d (%s)\n",
			target, depType, len(deps), depsTime.Nanosecond(), depState)
		for _, d := range deps {
			fmt.Fprintf(&buf, "    %s\n", d)
		}
		fmt.Fprintln(w, buf.String())
	}
	return w.Flush()
}

func lookupDeps(ctx context.Context, state *ninjautil.State, hashFS *hashfs.HashFS, depsLog *ninjautil.DepsLog, wd, target string) (string, []string, time.Time, string, error) {
	depState := "UNKNOWN"
	deps, depsTime, err := depsLog.Get(ctx, target)
	if err == nil {
		if hashFS != nil {
			depState = "STALE"
			fi, err := hashFS.Stat(ctx, wd, target)
			if err != nil {
				clog.Warningf(ctx, "%v", err)
				// log and ignore stat error
			} else {
				mtime := fi.ModTime()
				if !mtime.After(depsTime) {
					depState = "VALID"
				}
			}
		}
		return "deps", deps, depsTime, depState, err
	}
	if state == nil {
		return "", nil, time.Time{}, depState, ninjautil.ErrNoDepsLog
	}
	node, ok := state.LookupNodeByPath(target)
	if !ok {
		return "", nil, time.Time{}, depState, fmt.Errorf("no such target in build graph: %q", target)
	}
	edge, ok := node.InEdge()
	if !ok {
		return "", nil, time.Time{}, depState, fmt.Errorf("no rule to build target: %q", target)
	}
	depsType := edge.Binding("deps")
	switch depsType {
	case "gcc", "msvc":
		// for deps=gcc|msvc, deps is recorded in deps log.
		return "", nil, time.Time{}, depState, ninjautil.ErrNoDepsLog
	case "":
		// check depfile
	default:
		return "", nil, time.Time{}, depState, fmt.Errorf("unknown deps=%q in rule to build target %q", depsType, target)
	}
	depfile := edge.UnescapedBinding("depfile")
	if depfile == "" {
		// the rule has no deps,depfile.
		return "", nil, time.Time{}, depState, ninjautil.ErrNoDepsLog
	}
	fi, err := hashFS.Stat(ctx, wd, depfile)
	if err != nil {
		return "", nil, time.Time{}, depState, fmt.Errorf("no depfile=%q to build target %q: %w", depfile, target, err)
	}
	fsys := hashFS.FileSystem(ctx, wd)
	deps, err = makeutil.ParseDepsFile(ctx, fsys, depfile)
	if err != nil {
		return "", nil, time.Time{}, depState, fmt.Errorf("failed to read depfile=%q to build target %q: %w", depfile, target, err)
	}
	return fmt.Sprintf("depfile=%q", depfile), deps, fi.ModTime(), "VALID", nil
}

func depsTargets(ctx context.Context, state *ninjautil.State, depsLog *ninjautil.DepsLog, args []string) ([]string, error) {
	var nodes []*ninjautil.Node
	if len(args) > 0 {
		var err error
		nodes, err = state.Targets(args)
		if err != nil {
			return nil, err
		}
	} else {
		// for empty args, not use "defaults", but use all deps log entries.
		nodes = state.AllNodes()
		slices.SortFunc(nodes, func(a, b *ninjautil.Node) int {
			return strings.Compare(a.Path(), b.Path())
		})

	}
	targets := make([]string, 0, len(nodes))
	for _, node := range nodes {
		targets = append(targets, node.Path())
	}
	return targets, nil
}
