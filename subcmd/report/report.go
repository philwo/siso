// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package report is report subcommand to report siso logs.
package report

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"time"

	"github.com/maruel/subcommands"

	"go.chromium.org/luci/common/cli"
	"go.chromium.org/luci/common/system/signals"

	"infra/build/siso/o11y/clog"
	"infra/build/siso/o11y/iometrics"
	"infra/build/siso/reapi/digest"
	"infra/build/siso/ui"
)

const usage = `report siso logs
Collect siso logs in <dir>.

 $ siso report -C <dir>
`

// Cmd returns the Command for the `report` subcommand provided by this package.
func Cmd() *subcommands.Command {
	return &subcommands.Command{
		UsageLine: "report <args>...",
		ShortDesc: "report siso logs",
		LongDesc:  usage,
		CommandRun: func() subcommands.CommandRun {
			c := &run{}
			c.init()
			return c
		},
	}
}

type run struct {
	subcommands.CommandRunBase

	dir string
}

func (c *run) init() {
	c.Flags.StringVar(&c.dir, "C", ".", "ninja running directory")
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
	ctx, cancel := context.WithCancel(ctx)
	defer signals.HandleInterrupt(cancel)()

	clog.Infof(ctx, "dir %s", c.dir)
	err := os.Chdir(c.dir)
	if err != nil {
		return err
	}
	// TODO: upload report to make it easy to share.
	return c.archive(ctx)
}

func (c *run) collect(ctx context.Context) (map[string]digest.Data, error) {
	report := make(map[string]digest.Data)
	fsys := os.DirFS(".")
	var m iometrics.IOMetrics

	for _, pat := range []string{"siso*", ".siso*", "args.gn"} {
		matches, err := fs.Glob(fsys, pat)
		if err != nil {
			return nil, err
		}
		for _, fname := range matches {
			ui.Default.PrintLines(fmt.Sprintf("reading %s", fname))
			src := digest.LocalFileSource{Fname: fname, IOMetrics: &m}
			data, err := digest.FromLocalFile(ctx, src)
			if err != nil {
				clog.Errorf(ctx, "Error to calculate digest %s: %v", fname, err)
			} else {
				clog.Infof(ctx, "add %s %s", fname, data.Digest())
				report[fname] = data
			}
		}
	}
	_, err := os.Stat(".reproxy_tmp")
	if err != nil {
		clog.Infof(ctx, "no .reproxy_tmp: %v", err)
		return report, nil
	}
	err = fs.WalkDir(fsys, ".reproxy_tmp", func(fname string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		ui.Default.PrintLines(fmt.Sprintf("reading %s", fname))
		src := digest.LocalFileSource{Fname: fname, IOMetrics: &m}
		data, err := digest.FromLocalFile(ctx, src)
		if err != nil {
			clog.Errorf(ctx, "Error to calculate digest %s: %v", fname, err)
			return nil
		}
		clog.Infof(ctx, "add %s %s", fname, data.Digest())
		report[fname] = data
		return nil
	})
	return report, err
}

func (c *run) archive(ctx context.Context) (err error) {
	report, err := c.collect(ctx)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp("", "siso-report-*.tgz")
	if err != nil {
		return err
	}
	defer func() {
		cerr := f.Close()
		if err == nil {
			err = cerr
		}
	}()
	gw := gzip.NewWriter(f)
	defer func() {
		cerr := gw.Close()
		if err == nil {
			err = cerr
		}
	}()
	tw := tar.NewWriter(gw)
	defer func() {
		cerr := tw.Close()
		if err == nil {
			err = cerr
		}
	}()

	var fnames []string
	for fname := range report {
		fnames = append(fnames, fname)
	}
	sort.Strings(fnames)
	now := time.Now()
	for _, fname := range fnames {
		ui.Default.PrintLines(fmt.Sprintf("packing %s", fname))
		buf, err := digest.DataToBytes(ctx, report[fname])
		if err != nil {
			return fmt.Errorf("failed to get bytes for %s: %w", fname, err)
		}
		err = tw.WriteHeader(&tar.Header{
			Name:    fname,
			Size:    int64(len(buf)),
			Mode:    0644,
			ModTime: now,
		})
		if err != nil {
			return fmt.Errorf("failed to write header for %s: %w", fname, err)
		}
		_, err = tw.Write(buf)
		if err != nil {
			return fmt.Errorf("failed to write data of %s: %w", fname, err)
		}
	}
	ui.Default.PrintLines(fmt.Sprintf("report file: %s\n", f.Name()))
	return tw.Flush()
}
