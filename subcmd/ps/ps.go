// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package ps is ps subcommand to list up active steps of ninja build.
package ps

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/maruel/subcommands"

	"go.chromium.org/luci/common/cli"
	"go.chromium.org/luci/common/system/signals"

	"infra/build/siso/build"
	"infra/build/siso/ui"
)

func Cmd() *subcommands.Command {
	return &subcommands.Command{
		UsageLine: "ps [-C dir] [--stdout_url url]",
		ShortDesc: "display running steps of ninja build",
		LongDesc: `Display running steps of ninja build.

for local build
 $ siso ps [-C dir]

for buiders build
 $ siso ps --stdout_url <compile-step-stdout-URL>
`,
		CommandRun: func() subcommands.CommandRun {
			c := &run{}
			c.init()
			return c
		},
	}
}

type run struct {
	subcommands.CommandRunBase

	stdoutURL string
	dir       string
	n         int
	interval  time.Duration
	termui    bool
	loc       string
}

func (c *run) init() {
	c.Flags.StringVar(&c.stdoutURL, "stdout_url", "", "stdout streaming URL")
	c.Flags.StringVar(&c.dir, "C", "", "ninja running directory")
	c.Flags.IntVar(&c.n, "n", 0, "limit number of steps if it is positive")
	c.Flags.DurationVar(&c.interval, "interval", -1, "query interval if it is positive. default 1s on terminal")
}

type source interface {
	location() string
	text() string
	fetch(context.Context) ([]build.ActiveStepInfo, error)
}

func (c *run) Run(a subcommands.Application, args []string, env subcommands.Env) int {
	ctx := cli.GetContext(a, c, env)
	ctx, cancel := context.WithCancel(ctx)
	defer signals.HandleInterrupt(func() {
		cancel()
	})()

	u, ok := ui.Default.(*ui.TermUI)
	if ok {
		c.termui = true
		if c.n == 0 {
			c.n = u.Height() - 2
		}
		if c.interval < 0 {
			c.interval = 1 * time.Second
		}
	}

	var src source
	var err error
	if c.stdoutURL != "" {
		src, err = newStdoutURLSource(ctx, c.stdoutURL)
	} else {
		src, err = newLocalSource(ctx, c.dir)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	c.loc = src.location()
	ret := 0
	for {
		activeSteps, err := src.fetch(ctx)
		if err != nil {
			if c.termui {
				fmt.Fprintf(os.Stderr, "\033[H\033[J%s\n", err)
			} else {
				fmt.Fprintf(os.Stderr, "%s\n", err)
			}
			ret = 1
		} else {
			var lines []string
			if c.termui {
				// move to 0,0 and clear to the end of screen.
				lines = append(lines, fmt.Sprintf("\033[H\033[JSiso is running in %s", c.loc))
				lines = append(lines, fmt.Sprintf("%10s %9s %s", "DURATION", "PHASE", "DESC"))
			} else {
				lines = append(lines, "\f\n")
				lines = append(lines, fmt.Sprintf("%10s %9s %s\n", "DURATION", "PHASE", "DESC"))
			}
			c.render(ctx, lines, activeSteps)
		}
		if c.interval <= 0 {
			break
		}
		if errors.Is(err, io.EOF) {
			fmt.Println(src.text())
			c.termui = false
			ui.Default = ui.LogUI{}
			c.render(ctx, nil, activeSteps)
			return ret
		}
		select {
		case <-time.After(c.interval):
		case <-ctx.Done():
			return ret
		}
	}
	return ret
}

func (c *run) render(ctx context.Context, lines []string, activeSteps []build.ActiveStepInfo) {
	headings := len(lines)
	for _, as := range activeSteps {
		if c.termui {
			lines = append(lines, fmt.Sprintf("%10s %9s %s", as.Dur, as.Phase, as.Desc))
		} else {
			lines = append(lines, fmt.Sprintf("%10s %9s %s\n", as.Dur, as.Phase, as.Desc))
		}
		if c.n > 0 && len(lines) >= c.n {
			break
		}
	}
	lines = append(lines, fmt.Sprintf("steps=%d out of %d\n", len(lines)-headings, len(activeSteps)))
	ui.Default.PrintLines(lines...)
}
