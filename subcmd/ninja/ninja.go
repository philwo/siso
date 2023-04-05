// Copyright 2023 The Chromium Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package ninja implements the subcommand `ninja` which parses a `build.ninja` file and builds the requested targets.
package ninja

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/maruel/subcommands"
	"go.chromium.org/luci/auth"
	"go.chromium.org/luci/common/cli"
	"go.chromium.org/luci/common/system/signals"

	"infra/build/siso/auth/cred"
	"infra/build/siso/build"
	"infra/build/siso/build/ninjabuild"
	"infra/build/siso/o11y/clog"
	"infra/build/siso/sync/semaphore"
)

// Cmd returns the Command for the `ninja` subcommand provided by this package.
func Cmd(authOpts auth.Options) *subcommands.Command {
	return &subcommands.Command{
		UsageLine: "ninja <args>...",
		CommandRun: func() subcommands.CommandRun {
			r := ninjaCmdRun{
				authOpts: authOpts,
			}
			return &r
		},
	}
}

type ninjaCmdRun struct {
	subcommands.CommandRunBase
	authOpts auth.Options
}

// Run runs the `ninja` subcommand.
func (c *ninjaCmdRun) Run(a subcommands.Application, args []string, env subcommands.Env) int {
	ctx := cli.GetContext(a, c, env)
	ctx, cancel := context.WithCancel(ctx)
	defer signals.HandleInterrupt(cancel)()

	_, err := cred.New(ctx, c.authOpts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Failed to authenticate. Please login with `siso login`.")
		return 1
	}

	// Use au.PerRPCCredentials() to get PerRPCCredentials of google.golang.org/grpc/credentials.
	// Use au.TokenSource() to get oauth2.TokenSource.
	return 0
}

func doBuild(ctx context.Context, graph *ninjabuild.Graph, bopts build.Options, args ...string) error {
	clog.Infof(ctx, "rebuild manifest")
	mfbopts := bopts
	mfbopts.Clobber = false
	mfbopts.RebuildManifest = graph.Filename()
	mfb, err := build.New(ctx, graph, mfbopts)
	if err != nil {
		return err
	}
	err = mfb.Build(ctx, "rebuild manifest", graph.Filename())
	if err != nil {
		mfb.Close(ctx)
		return err
	}
	// TODO(b/266518906): upload manifest

	b, err := build.New(ctx, graph, bopts)
	if err != nil {
		return err
	}
	defer b.Close(ctx)
	// prof := newCPUProfiler(ctx, "build")
	err = b.Build(ctx, "build", args...)
	// prof.stop(ctx)

	semaTraces := make(map[string]semaTrace)
	tstats := b.TraceStats()
	var rbeWorker, rbeExec *build.TraceStat
	for _, ts := range tstats {
		clog.Infof(ctx, "%s: n=%d avg=%s max=%s", ts.Name, ts.N, ts.Avg(), ts.Max)
		switch {
		case strings.HasPrefix(ts.Name, "wait:"):
			name := strings.TrimPrefix(ts.Name, "wait:")
			t := semaTraces[name]
			t.name = name
			t.n = ts.N
			t.waitAvg = ts.Avg()
			t.waitBuckets = ts.Buckets
			semaTraces[name] = t
		case strings.HasPrefix(ts.Name, "serv:"):
			name := strings.TrimPrefix(ts.Name, "serv:")
			t := semaTraces[name]
			t.name = name
			t.n = ts.N
			t.servAvg = ts.Avg()
			t.servBuckets = ts.Buckets
			semaTraces[name] = t
		case ts.Name == "rbe:queue":
			name := "rbe:sched"
			t := semaTraces[name]
			t.name = name
			t.n = ts.N
			t.waitAvg = ts.Avg()
			t.waitBuckets = ts.Buckets
			semaTraces[name] = t
		case ts.Name == "rbe:worker":
			rbeWorker = ts
		case ts.Name == "rbe:exec":
			rbeExec = ts
		}
	}
	if rbeWorker != nil {
		name := "rbe:sched"
		t := semaTraces[name]
		t.name = name
		t.servAvg = rbeWorker.Avg()
		t.servBuckets = rbeWorker.Buckets
		semaTraces[name] = t
	}
	if rbeWorker != nil && rbeExec != nil {
		name := "rbe:worker"
		t := semaTraces[name]
		t.name = name
		t.n = rbeExec.N
		t.waitAvg = rbeWorker.Avg() - rbeExec.Avg()
		// number of waits would not be correct with this calculation
		// because it just uses counts in buckets.
		// not sure how we can measure actual waiting time in buckets,
		// but this would provide enough estimated values.
		for i := range rbeWorker.Buckets {
			t.waitBuckets[i] = rbeWorker.Buckets[i] - rbeExec.Buckets[i]
		}
		t.servAvg = rbeExec.Avg()
		t.servBuckets = rbeExec.Buckets
		semaTraces[name] = t
	}
	if len(semaTraces) > 0 {
		var semaNames []string
		for key := range semaTraces {
			semaNames = append(semaNames, key)
		}
		sort.Strings(semaNames)
		tw := tabwriter.NewWriter(os.Stdout, 10, 8, 1, ' ', tabwriter.AlignRight)
		fmt.Fprintf(tw, "resource/capa\tused\twait-avg\t|   s m |\tserv-avg\t|   s m |\t\n")
		for _, key := range semaNames {
			t := semaTraces[key]
			s, _ := semaphore.Lookup(t.name)
			c := "nil"
			if s != nil {
				c = strconv.Itoa(s.Capacity())
			}
			fmt.Fprintf(tw, "%s/%s\t%d\t%s\t%s\t%s\t%s\t\n", t.name, c, t.n, t.waitAvg.Round(time.Millisecond), histogram(t.waitBuckets), t.servAvg.Round(time.Millisecond), histogram(t.servBuckets))
		}
		tw.Flush()
	}
	if bopts.REAPIClient == nil {
		return err
	}

	// TODO(b/266518906): wait for completion of uploading manifest
	return err
}

var histchar = [...]string{"▂", "▃", "▄", "▅", "▆", "▇", "█"}

func histogram(b [7]int) string {
	max := 0
	for _, n := range b {
		if max < n {
			max = n
		}
	}
	var sb strings.Builder
	sb.WriteRune('|')
	for _, n := range b {
		if n <= 0 {
			sb.WriteRune(' ')
			continue
		}
		i := len(histchar) * n / (max + 1)
		sb.WriteString(histchar[i])
	}
	sb.WriteRune('|')
	return sb.String()
}

type semaTrace struct {
	name                     string
	n                        int
	waitAvg, servAvg         time.Duration
	waitBuckets, servBuckets [7]int
}
