// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package metricssummary is metricssummary subcommand to summarize siso_metrics.json.
package metricssummary

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/maruel/subcommands"
	"go.chromium.org/luci/common/cli"

	"infra/build/siso/build"
	"infra/build/siso/ui"
)

const usage = `summarize siso_metrics.json

 $ siso metricssummary -C <dir> \
    [--step_types <types>] \
    [--elapsed_time_sorting] \
    [--input siso_metrics.json]

summarize <dir>/.siso_metrics.json (--input)
as depot_tools/post_ninja_build_summary.py does.
`

// Cmd returns the Command for the `metricssummary` subcommand provided by this package.
func Cmd() *subcommands.Command {
	return &subcommands.Command{
		UsageLine: "metricssummary <args>...",
		ShortDesc: "summarize siso_metrics.json",
		LongDesc:  usage,
		Advanced:  true,
		CommandRun: func() subcommands.CommandRun {
			c := &run{}
			c.init()
			return c
		},
	}
}

type run struct {
	subcommands.CommandRunBase

	dir                string
	input              string
	stepTypes          string
	elapsedTimeSorting bool
}

func (c *run) init() {
	c.Flags.StringVar(&c.dir, "C", ".", "ninja running directory, where siso_metrics.json exists")
	c.Flags.StringVar(&c.input, "input", "siso_metrics.json", "filename of siso_metrics.json to summarize")
	c.Flags.StringVar(&c.stepTypes, "step_types", "", "semicolon separated glob patterns (go filepath.Match) for build-step grouping")
	c.Flags.BoolVar(&c.elapsedTimeSorting, "elapsed_time_sorting", false, "Sort output by elapsed time instead of weighted time")
}

func (c *run) Run(a subcommands.Application, args []string, env subcommands.Env) int {
	ctx := cli.GetContext(a, c, env)
	err := c.run(ctx)
	if err != nil {
		switch {
		case errors.Is(err, flag.ErrHelp):
			fmt.Fprintf(os.Stderr, "%v\n%s\n", err, usage)
		default:
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		return 1
	}
	return 0
}

func (c *run) run(ctx context.Context) error {
	err := os.Chdir(c.dir)
	if err != nil {
		return err
	}
	metrics, err := loadMetrics(ctx, c.input)
	if err != nil {
		return err
	}

	if len(metrics) == 0 {
		return fmt.Errorf("no metrics data?")
	}

	// TODO(ukai): deduce wait time from the duration?

	var totalTime time.Duration
	var accumulatedDuration time.Duration
	var m []build.StepMetric
	for _, s := range metrics {
		if s.StepID == "" {
			// this is special entry for build metrics, not per step metrics.
			totalTime = time.Duration(s.Duration)
			continue
		}
		m = append(m, s)
		accumulatedDuration += time.Duration(s.Duration)
	}
	metrics = m

	// Print the slowest build steps:
	fmt.Println("    Longest build steps:")
	if c.elapsedTimeSorting {
		sort.Slice(metrics, func(i, j int) bool {
			return metrics[i].Duration > metrics[j].Duration
		})
	} else {
		sort.Slice(metrics, func(i, j int) bool {
			return metrics[i].WeightedDuration > metrics[j].WeightedDuration
		})
	}
	longCount := 10
	m = metrics
	if len(m) > longCount {
		m = m[:longCount]
	}
	for _, s := range m {
		fmt.Printf("      %8s weighted to build %s (%s elapsed time)\n",
			ui.FormatDuration(time.Duration(s.WeightedDuration)),
			relPath(c.dir, s.Output),
			ui.FormatDuration(time.Duration(s.Duration)))
	}

	// Sum up the time by file extension/type of the output file
	stepTypes := strings.Split(c.stepTypes, ";")
	am, err := aggregate(metrics, stepTypes)
	if err != nil {
		return err
	}
	fmt.Println("    Time by build-step type:")
	if c.elapsedTimeSorting {
		sort.Slice(am, func(i, j int) bool {
			return am[i].Duration > am[j].Duration
		})
	} else {
		sort.Slice(am, func(i, j int) bool {
			return am[i].WeightedDuration > am[j].WeightedDuration
		})
	}
	// Print the slowest build target types:
	if len(am) > longCount+len(stepTypes) {
		am = am[:longCount+len(stepTypes)]
	}
	for _, s := range am {
		fmt.Printf("      %8s weighted to generate %d %s files (%s elapsed time sum)\n",
			ui.FormatDuration(s.WeightedDuration),
			s.Count,
			s.Type,
			ui.FormatDuration(s.Duration))
	}
	if totalTime == 0 {
		return nil
	}
	fmt.Printf("    %s weighted time (%s elapsed time sum, %1.1fx parallelism)\n",
		ui.FormatDuration(totalTime),
		ui.FormatDuration(accumulatedDuration),
		accumulatedDuration.Seconds()/totalTime.Seconds())
	fmt.Printf("    %d build steps completed, average of %1.2f/s\n",
		len(metrics),
		float64(len(metrics))/totalTime.Seconds())
	return nil
}

func loadMetrics(ctx context.Context, fname string) ([]build.StepMetric, error) {
	f, err := os.Open(fname)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	d := json.NewDecoder(f)
	var metrics []build.StepMetric
	for {
		var m build.StepMetric
		err := d.Decode(&m)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parse error in %s:%d: %w", fname, d.InputOffset(), err)
		}
		metrics = append(metrics, m)
	}
	return metrics, nil
}

type aggregatedMetric struct {
	Type             string
	Count            int
	Duration         time.Duration
	WeightedDuration time.Duration
}

func aggregate(metrics []build.StepMetric, pats []string) ([]aggregatedMetric, error) {
	am := make(map[string]aggregatedMetric)
	for _, m := range metrics {
		if m.StepID == "" {
			continue
		}
		t, err := stepType(m, pats)
		if err != nil {
			return nil, err
		}
		a := am[t]
		a.Type = t
		a.Count++
		a.Duration += time.Duration(m.Duration)
		a.WeightedDuration += time.Duration(m.WeightedDuration)
		am[t] = a
	}
	var ret []aggregatedMetric
	for _, v := range am {
		ret = append(ret, v)
	}
	return ret, nil
}

func relPath(base, fname string) string {
	r, err := filepath.Rel(base, fname)
	if err != nil {
		return fname
	}
	return r
}

func stepType(metric build.StepMetric, pats []string) (string, error) {
	for _, p := range pats {
		ok, err := filepath.Match(p, metric.Output)
		if err != nil {
			return "", err
		}
		if ok {
			return p, nil
		}
	}
	return filepath.Ext(metric.Output), nil
}
