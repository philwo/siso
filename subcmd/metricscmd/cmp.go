// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package metricscmd

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/maruel/subcommands"

	"go.chromium.org/luci/common/cli"

	"go.chromium.org/infra/build/siso/build"
)

const cmpUsage = `compare siso_metrics.json.

 $ siso metrics cmp -C <dir> [--format <format>] \
   [--input_a siso_metrics.json] \
   [--input_b siso_metrics.json.0]

compares <dir>/siso_metrics.json (--input_a) and
<dir>/siso_metrics.json.0 (--input_b).

output format can be "diff" or "join".

diff shows duration difference: input_a - input_b
 to see difference for each duration per outputs,
 so what outputs became slower or what step of the outputs became slower
 than before.
join makes a pair of metrics for each output.
 to see difference per outputs,
 e.g. check cmdhash, action has been changed etc.

default output is diff.
`

// Cmd returns the Command for the `metricscmp` subcommand provided by this package.
func cmpCmd() *subcommands.Command {
	return &subcommands.Command{
		UsageLine: "cmp <args>...",
		ShortDesc: "compare siso_metrics.json",
		LongDesc:  cmpUsage,
		CommandRun: func() subcommands.CommandRun {
			c := &cmpRun{}
			c.init()
			return c
		},
	}
}

type cmpRun struct {
	subcommands.CommandRunBase

	dir            string
	inputA, inputB string
	format         string
}

var formats = map[string]func([]build.StepMetric) error{
	"diff": outputDiff,
	"join": outputJoin,
}

var formatKeys = func() []string {
	var keys []string
	for k := range formats {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}()

func (c *cmpRun) init() {
	c.Flags.StringVar(&c.dir, "C", ".", "ninja running directory")
	c.Flags.StringVar(&c.inputA, "input_a", "siso_metrics.json", "target siso_metrics.json")
	c.Flags.StringVar(&c.inputB, "input_b", "siso_metrics.json.0", "base siso_metrics.json")

	c.Flags.StringVar(&c.format, "format", "diff", fmt.Sprintf("output format: %q", formatKeys))
}

func (c *cmpRun) Run(a subcommands.Application, args []string, env subcommands.Env) int {
	ctx := cli.GetContext(a, c, env)
	err := c.run(ctx)
	if err != nil {
		switch {
		case errors.Is(err, flag.ErrHelp):
			fmt.Fprintf(os.Stderr, "%v\n%s\n", err, cmpUsage)
		default:
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		return 1
	}
	return 0
}

func (c *cmpRun) run(ctx context.Context) error {
	output, ok := formats[c.format]
	if !ok {
		return fmt.Errorf("unknown format %q: known formats %q: %w", c.format, formatKeys, flag.ErrHelp)
	}

	x, err := loadMetrics(ctx, filepath.Join(c.dir, c.inputA))
	if err != nil {
		return err
	}
	y, err := loadMetrics(ctx, filepath.Join(c.dir, c.inputB))
	if err != nil {
		return err
	}
	z := join(x, y)
	for _, m := range z {
		err := output(m)
		if err != nil {
			return err
		}
	}
	return nil
}

func outputJoin(m []build.StepMetric) error {
	buf, err := json.Marshal(m)
	if err != nil {
		return err
	}
	fmt.Printf("%s\n", buf)
	return nil
}

func outputDiff(m []build.StepMetric) error {
	d := struct {
		Action string `json:"action"`
		Rule   string `json:"rule,omitempty"`
		Output string `json:"output"`

		Ready            build.IntervalMetric `json:"ready"`
		Start            build.IntervalMetric `json:"start"`
		Duration         build.IntervalMetric `json:"duration"`
		WeightedDuration build.IntervalMetric `json:"weighted_duration"`
		RunTime          build.IntervalMetric `json:"run"`
		QueueTime        build.IntervalMetric `json:"queue"`
		ExecTime         build.IntervalMetric `json:"exec"`
	}{
		Action:           m[0].Action,
		Output:           m[0].Output,
		Ready:            m[0].Ready - m[1].Ready,
		Start:            m[0].Start - m[1].Start,
		Duration:         m[0].Duration - m[1].Duration,
		WeightedDuration: m[0].WeightedDuration - m[1].WeightedDuration,
		RunTime:          m[0].RunTime - m[1].RunTime,
		QueueTime:        m[0].QueueTime - m[1].QueueTime,
		ExecTime:         m[0].ExecTime - m[1].ExecTime,
	}
	buf, err := json.Marshal(d)
	if err != nil {
		return err
	}
	fmt.Printf("%s\n", buf)
	return nil
}

func join(x, y []build.StepMetric) [][]build.StepMetric {
	xm := indexMap(x)
	ym := indexMap(y)

	km := map[string]bool{}
	var keys []string
	for k := range xm {
		keys = append(keys, k)
		km[k] = true
	}
	for k := range ym {
		if km[k] {
			continue
		}
		keys = append(keys, k)
		km[k] = true
	}
	sort.Strings(keys)
	var r [][]build.StepMetric
	for _, k := range keys {
		r = append(r, []build.StepMetric{xm[k], ym[k]})
	}
	return r
}

func indexMap(x []build.StepMetric) map[string]build.StepMetric {
	r := make(map[string]build.StepMetric)
	for _, m := range x {
		output := m.Output
		r[output] = m
	}
	return r
}
