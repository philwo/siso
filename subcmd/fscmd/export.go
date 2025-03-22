// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package fscmd

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/maruel/subcommands"
	"google.golang.org/protobuf/encoding/prototext"

	"go.chromium.org/infra/build/siso/hashfs"
)

func cmdFSExport() *subcommands.Command {
	return &subcommands.Command{
		UsageLine: "export",
		ShortDesc: "export siso hashfs data",
		LongDesc:  "export siso hashfs data to stdout.",
		CommandRun: func() subcommands.CommandRun {
			c := &exportRun{}
			c.init()
			return c
		},
	}
}

type exportRun struct {
	subcommands.CommandRunBase
	dir       string
	format    string
	stateFile string
}

func (c *exportRun) init() {
	c.Flags.StringVar(&c.dir, "C", ".", "ninja running directory")
	c.Flags.StringVar(&c.format, "format", "json", "output format. json or prototext")
	c.Flags.StringVar(&c.stateFile, "fs_state", stateFile, "fs state filename")
}

func (c *exportRun) Run(a subcommands.Application, args []string, env subcommands.Env) int {
	err := os.Chdir(c.dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to chdir %s: %v\n", c.dir, err)
		return 1
	}

	st, err := hashfs.Load(hashfs.Option{StateFile: c.stateFile})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load %s: %v\n", c.stateFile, err)
		return 1
	}
	var buf []byte
	switch c.format {
	case "json":
		buf, err = json.MarshalIndent(st, "", " ")
	case "prototext":
		buf, err = prototext.MarshalOptions{
			Multiline: true,
			Indent:    " ",
		}.Marshal(st)
	default:
		fmt.Fprintf(os.Stderr, "unknown format %s\n", c.format)
		return 2
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshal error: %v\n", err)
		return 1
	}
	os.Stdout.Write(buf)
	return 0
}
