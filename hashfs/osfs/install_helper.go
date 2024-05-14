// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package osfs

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/maruel/subcommands"

	"go.chromium.org/luci/common/cli"
)

func HelperCmd() *subcommands.Command {
	return &subcommands.Command{
		UsageLine: "install-helper [-m mode] [-o file]",
		ShortDesc: "helper tool to install executable",
		LongDesc: `helper tool to install executable.

User would not need to run this sub command.
It is intended as workaround for https://github.com/golang/go/issues/22315.
This tool just writes new executable in specified file with mode
by using content given in stdin.
`,
		Advanced: true,
		CommandRun: func() subcommands.CommandRun {
			c := &run{}
			c.init()
			return c
		},
	}
}

type run struct {
	subcommands.CommandRunBase

	mode int
	file string
}

func (c *run) init() {
	c.Flags.IntVar(&c.mode, "m", 0, "file mode")
	c.Flags.StringVar(&c.file, "o", "", "output filename")
}

func (c *run) Run(a subcommands.Application, args []string, env subcommands.Env) int {
	ctx := cli.GetContext(a, c, env)
	err := c.run(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	return 0
}

func (c *run) run(ctx context.Context) error {
	if c.mode&0700 == 0 {
		return fmt.Errorf("invalid mode 0%o: %s", c.mode, fs.FileMode(c.mode))
	}
	if c.file == "" {
		return fmt.Errorf("file not specified")
	}
	w, err := os.OpenFile(c.file, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, os.FileMode(c.mode))
	if err != nil {
		return err
	}
	_, err = io.Copy(w, os.Stdin)
	cerr := w.Close()
	if err != nil {
		return err
	}
	if cerr != nil {
		return cerr
	}
	return nil
}
