// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package scandeps is scandeps subcommand for debugging scandeps.
package scandeps

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/charmbracelet/log"
	"github.com/maruel/subcommands"

	"go.chromium.org/luci/common/cli"

	"go.chromium.org/infra/build/siso/build/buildconfig"
	"go.chromium.org/infra/build/siso/build/ninjabuild"
	"go.chromium.org/infra/build/siso/hashfs"
	"go.chromium.org/infra/build/siso/scandeps"
)

const usage = `run scandeps

 $ siso scandeps -C <dir> -req '<json scandeps request>'

<json scandeps request> can be found in siso.INFO log
for "scandeps failed Request". you can copy-and-paste
the json string from the log.
Or you can manually construct json string of
infra/build/siso/scandeps.Request.
`

// Cmd returns the Command for the `scandeps` subcommand provided by this package.
func Cmd() *subcommands.Command {
	return &subcommands.Command{
		UsageLine: "scandeps <args>...",
		ShortDesc: "run scandeps",
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

	dir       string
	reqString string
}

func (c *run) init() {
	c.Flags.StringVar(&c.dir, "C", ".", "ninja running directory to find .siso_config and .siso_filegroup for input_deps")
	c.Flags.StringVar(&c.reqString, "req", "", "json format of scandeps request")
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
	if c.reqString == "" {
		return fmt.Errorf("missing req: %w", flag.ErrHelp)
	}
	var req scandeps.Request
	err := json.Unmarshal([]byte(c.reqString), &req)
	if err != nil {
		return err
	}
	fmt.Printf("request=%#v\n", req)
	inputDeps, err := loadInputDeps(c.dir)
	if err != nil {
		return err
	}
	log.Infof("input_deps=%q\n", inputDeps)

	execRoot, err := os.Getwd()
	if err != nil {
		return err
	}
	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		return err
	}

	s := scandeps.New(hashFS, inputDeps)

	result, err := s.Scan(ctx, execRoot, req)
	if err != nil {
		return err
	}
	for _, r := range result {
		fmt.Println(r)
	}
	return nil
}

func loadInputDeps(dir string) (map[string][]string, error) {
	buf, err := os.ReadFile(filepath.Join(dir, ".siso_config"))
	if err != nil {
		return nil, err
	}
	var stepConfig ninjabuild.StepConfig
	err = json.Unmarshal(buf, &stepConfig)
	if err != nil {
		return nil, fmt.Errorf("load %s/.siso_config: %w", dir, err)
	}
	inputDeps := stepConfig.InputDeps

	buf, err = os.ReadFile(filepath.Join(dir, ".siso_filegroups"))
	if err != nil {
		return nil, err
	}
	var filegroups buildconfig.Filegroups
	err = json.Unmarshal(buf, &filegroups)
	if err != nil {
		return nil, fmt.Errorf("load %s/.filegroups: %w", dir, err)
	}
	for k, v := range filegroups.Filegroups {
		inputDeps[k] = v
	}
	return inputDeps, nil
}
