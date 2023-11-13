// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package fetch is fetch subcommand to fetch data from CAS.
package fetch

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/maruel/subcommands"
	"google.golang.org/protobuf/proto"

	"go.chromium.org/luci/auth"
	"go.chromium.org/luci/common/cli"
	"go.chromium.org/luci/common/system/signals"

	"infra/build/siso/auth/cred"
	"infra/build/siso/reapi"
	"infra/build/siso/reapi/digest"
	"infra/build/siso/reapi/merkletree/exporter"
)

const usage = `fetch contents from CAS.
Print contents to stdout, or extract in <dir> for -type dir-extract.

 $ siso fetch -project <project> -reapi_instnace <instnace> \
          [-type <type>] \
          <digest> [<dir>]

<type> is
  raw: raw content
  command: command message in text proto format
  action: action message in text proto format
  dir: directory message in text proto format
  tree: tree message in text proto format
  dir-extract: extract to <dir> (if <dir> is specified)
               or list (if <dir> is not specified)
`

// Cmd returns the Command for the `fetch` subcommand provided by this package.
func Cmd(authOpts cred.Options) *subcommands.Command {
	return &subcommands.Command{
		UsageLine: "fetch <args>...",
		ShortDesc: "fetch contents",
		LongDesc:  usage,
		CommandRun: func() subcommands.CommandRun {
			c := &run{
				authOpts: authOpts,
			}
			c.init()
			return c
		},
	}
}

type run struct {
	subcommands.CommandRunBase

	authOpts  cred.Options
	projectID string
	reopt     *reapi.Option
	dataType  string
}

func (c *run) init() {
	c.Flags.StringVar(&c.projectID, "project", os.Getenv("SISO_PROJECT"), "cloud project ID. can be set by $SISO_PROJECT")
	c.reopt = new(reapi.Option)
	envs := map[string]string{
		"SISO_REAPI_ADDRESS":  os.Getenv("SISO_REAPI_ADDRESS"),
		"SISO_REAPI_INSTANCE": os.Getenv("SISO_REAPI_INSTANCE"),
	}
	c.reopt.RegisterFlags(&c.Flags, envs)
	c.Flags.StringVar(&c.dataType, "type", "raw", `data type. "raw", "command", "action", "dir", "tree", "dir-extract"`)
}

func (c *run) Run(a subcommands.Application, args []string, env subcommands.Env) int {
	ctx := cli.GetContext(a, c, env)
	err := c.run(ctx)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrLoginRequired):
			fmt.Fprintf(os.Stderr, "need to login: run `siso login`\n")
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

	projectID := c.reopt.UpdateProjectID(c.projectID)
	var credential cred.Cred
	var err error
	if projectID == "" {
		return errors.New("project ID is not specified")
	}
	credential, err = cred.New(ctx, c.authOpts)
	if err != nil {
		return err
	}
	if c.Flags.NArg() == 0 {
		return fmt.Errorf("no digest: %w", flag.ErrHelp)
	}
	d, err := digest.Parse(c.Flags.Arg(0))
	if err != nil {
		return err
	}
	client, err := reapi.New(ctx, credential, *c.reopt)
	if err != nil {
		return err
	}
	defer client.Close()

	var pmsg proto.Message
	switch c.dataType {
	case "raw":
	case "command":
		pmsg = &rpb.Command{}
	case "action":
		pmsg = &rpb.Action{}
	case "dir":
		pmsg = &rpb.Directory{}
	case "tree":
		pmsg = &rpb.Tree{}
	case "dir-extract":
		var w io.Writer
		dir := "."
		if c.Flags.NArg() > 1 {
			dir = c.Flags.Arg(1)
			fmt.Printf("extract %s to %s\n", d, dir)
		} else {
			w = os.Stdout
			fmt.Printf("list %s\n", d)
		}

		b, err := client.Get(ctx, d, d.String())
		if err != nil {
			return err
		}
		pmsg := &rpb.Directory{}
		err = c.protoUnmarshal(b, pmsg)
		if err != nil {
			return fmt.Errorf("failed to unmarshal %s as %T: %v", d, pmsg, err)
		}
		exporter := exporter.New(client)
		err = exporter.Export(ctx, dir, d, w)
		if err != nil {
			return fmt.Errorf("error from exporter.Export: %w", err)
		}
		return nil
	default:
		var w strings.Builder
		c.Flags.SetOutput(&w)
		c.Flags.PrintDefaults()
		return fmt.Errorf("unknown type %s\n%s\n%w", c.dataType, w.String(), flag.ErrHelp)
	}
	b, err := client.Get(ctx, d, d.String())
	if err != nil {
		return fmt.Errorf("error from client.Get: %w", err)
	}
	if pmsg == nil {
		_, err = os.Stdout.Write(b)
		if err != nil {
			return err
		}
		return nil
	}
	err = c.protoUnmarshal(b, pmsg)
	if err != nil {
		return fmt.Errorf("failed to unmarshal %s as %T: %v", d, pmsg, err)
	}
	fmt.Println(pmsg)
	return nil
}

func (c *run) protoUnmarshal(b []byte, msg proto.Message) error {
	err := proto.Unmarshal(b, msg)
	if err != nil {
		return err
	}
	unknown := msg.ProtoReflect().GetUnknown()
	if len(unknown) > 0 {
		return fmt.Errorf("unknown fields in marshaled proto: %v", msg)
	}
	return nil
}
