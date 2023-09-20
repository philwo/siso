// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package fscmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/maruel/subcommands"
	"google.golang.org/protobuf/encoding/prototext"

	"go.chromium.org/luci/common/cli"

	"infra/build/siso/hashfs"
	pb "infra/build/siso/hashfs/proto"
)

func cmdFSImport() *subcommands.Command {
	return &subcommands.Command{
		UsageLine: "import",
		ShortDesc: "import siso hashfs data",
		LongDesc:  "import siso hashfs data from stdin.",
		Advanced:  true,
		CommandRun: func() subcommands.CommandRun {
			c := &importRun{}
			c.init()
			return c
		},
	}
}

type importRun struct {
	subcommands.CommandRunBase
	dir    string
	format string
}

func (c *importRun) init() {
	c.Flags.StringVar(&c.dir, "C", ".", "ninja running directory")
	c.Flags.StringVar(&c.format, "format", "json", "input format. json or prototext")
}

func (c *importRun) Run(a subcommands.Application, args []string, env subcommands.Env) int {
	ctx := cli.GetContext(a, c, env)

	err := os.Chdir(c.dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to chdir %s: %v\n", c.dir, err)
		return 1
	}

	buf, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to read from stdin: %v\n", err)
		return 1
	}

	st := &pb.State{}
	switch c.format {
	case "json":
		err = json.Unmarshal(buf, st)
	case "prototext":
		err = prototext.Unmarshal(buf, st)
	default:
		fmt.Fprintf(os.Stderr, "unknown format %s\n", c.format)
		return 2
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "unmarshal error: %v\n", err)
		return 1
	}
	os.Remove(".siso_last_targets")
	err = hashfs.Save(ctx, stateFile, st)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to save %s: %v\n", stateFile, err)
		return 1
	}
	return 0
}
