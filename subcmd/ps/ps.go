// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package ps is ps subcommand to list up active steps of ninja build.
package ps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
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
		UsageLine: "ps [-C dir]",
		ShortDesc: "display running steps of ninja build",
		LongDesc: `Display running steps of ninja build.

 $ siso ps [-C dir]
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

	dir      string
	n        int
	interval time.Duration
	termui   bool
	wd       string
}

func (c *run) init() {
	c.Flags.StringVar(&c.dir, "C", "", "ninja running directory")
	c.Flags.IntVar(&c.n, "n", 0, "limit number of steps if it is positive")
	c.Flags.DurationVar(&c.interval, "interval", -1, "query interval if it is positive. default 1s on terminal")
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

	err := os.Chdir(c.dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to chdir %s: %v\n", c.dir, err)
		return 1
	}
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to get wd: %v\n", err)
		return 1
	}
	c.wd = wd
	ret := 0
	for {
		buf, err := os.ReadFile(".siso_port")
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				if c.termui {
					fmt.Fprintf(os.Stderr, "\033[H\033[JSiso is not running in %s?\n", c.wd)
				} else {
					fmt.Fprintf(os.Stderr, "Siso is not running in %s?\n", c.wd)
				}

			} else {
				if c.termui {
					fmt.Fprintf(os.Stderr, "\033[H\033[JSiso is not running in %s? failed to read .siso_port: %v\n", c.wd, err)
				} else {
					fmt.Fprintf(os.Stderr, "Siso is not running in %s? failed to read .siso_port: %v\n", c.wd, err)
				}
			}
			ret = 1
		} else {
			err = c.run(ctx, string(buf))
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to get info: %v\n", err)
				ret = 1
			}
		}
		if c.interval <= 0 {
			break
		}
		select {
		case <-time.After(c.interval):
		case <-ctx.Done():
			return ret
		}
	}
	return ret
}

func (c *run) run(ctx context.Context, addr string) error {
	resp, err := http.Get(fmt.Sprintf("http://%s/api/active_steps", addr))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("/api/active_steps error: %d %s", resp.StatusCode, resp.Status)
	}
	buf, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("/api/active_steps read error: %w", err)
	}
	var activeSteps []build.ActiveStepInfo
	err = json.Unmarshal(buf, &activeSteps)
	if err != nil {
		return fmt.Errorf("/api/active_steps unmarshal error: %w", err)
	}
	var lines []string
	if c.termui {
		// move to 0,0 and clear to the end of screen.
		lines = append(lines, fmt.Sprintf("\033[H\033[JSiso is running in %s", c.wd))
		lines = append(lines, fmt.Sprintf("%10s %9s %s", "DURATION", "PHASE", "DESC"))
	} else {
		lines = append(lines, "\f\n")
		lines = append(lines, fmt.Sprintf("%10s %9s %s\n", "DURATION", "PHASE", "DESC"))
	}
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
	lines = append(lines, fmt.Sprintf("steps=%d out of %d\n", len(lines)-2, len(activeSteps)))
	ui.Default.PrintLines(lines...)
	return nil
}
