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
	"strings"
	"time"

	"github.com/charmbracelet/log"
	"github.com/maruel/subcommands"

	"go.chromium.org/luci/common/cli"
	"go.chromium.org/luci/common/system/signals"

	"go.chromium.org/infra/build/siso/hashfs/osfs"
	"go.chromium.org/infra/build/siso/reapi/digest"
	"go.chromium.org/infra/build/siso/ui"
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

	dir     string
	osfsopt osfs.Option
}

func (c *run) init() {
	c.Flags.StringVar(&c.dir, "C", ".", "ninja running directory")
	c.osfsopt.RegisterFlags(&c.Flags)
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
	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	osfs := osfs.New("fs", c.osfsopt)

	for _, pat := range []string{"siso*", ".siso*", "args.gn", "gn_logs.txt"} {
		matches, err := fs.Glob(fsys, pat)
		if err != nil {
			return nil, err
		}
		if len(matches) == 0 {
			return nil, fmt.Errorf("no siso files in %s: did you specify correct `-C <dir>` ?", wd)
		}
		for _, fname := range matches {
			_, err := os.Stat(fname)
			if errors.Is(err, fs.ErrNotExist) {
				// dangling symlink or so?
				continue
			}
			ui.Default.PrintLines(fmt.Sprintf("reading %s", fname))
			localFname := fname
			if strings.HasSuffix(fname, ".redirected") {
				buf, err := os.ReadFile(fname)
				if err != nil {
					log.Warnf("failed to read %s: %v", fname, err)
					continue
				}
				localFname = string(buf)
				fname = strings.TrimSuffix(fname, ".redirected")
			}
			src := osfs.FileSource(localFname, -1)
			data, err := digest.FromLocalFile(ctx, src)
			if err != nil {
				log.Errorf("Error to calculate digest %s: %v", fname, err)
			} else {
				report[fname] = data
			}
		}
	}

	return report, nil
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
	ui.Default.PrintLines(fmt.Sprintf("report file: %s", f.Name()))
	return tw.Flush()
}
