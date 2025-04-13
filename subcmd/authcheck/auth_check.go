// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package authcheck provides auth_check subcommand.
package authcheck

import (
	"fmt"
	"os"

	"github.com/charmbracelet/log"
	"github.com/maruel/subcommands"
	"golang.org/x/oauth2"

	"go.chromium.org/luci/common/cli"

	"go.chromium.org/infra/build/siso/auth/cred"
	"go.chromium.org/infra/build/siso/reapi"
)

func Cmd(ts oauth2.TokenSource) *subcommands.Command {
	return &subcommands.Command{
		UsageLine: "auth-check",
		ShortDesc: "prints current auth status.",
		LongDesc:  "Prints current auth status.",
		CommandRun: func() subcommands.CommandRun {
			r := &authCheckRun{ts: ts}
			r.init()
			return r
		},
	}
}

type authCheckRun struct {
	subcommands.CommandRunBase
	ts        oauth2.TokenSource
	projectID string
	reopt     *reapi.Option
}

func (r *authCheckRun) init() {
	r.Flags.StringVar(&r.projectID, "project", os.Getenv("SISO_PROJECT"), "cloud project ID. can set by $SISO_PROJECT")

	r.reopt = new(reapi.Option)
	r.reopt.RegisterFlags(&r.Flags, reapi.Envs("REAPI"))
}

func (r *authCheckRun) Run(a subcommands.Application, args []string, env subcommands.Env) int {
	ctx := cli.GetContext(a, r, env)
	if len(args) != 0 {
		log.Errorf("%s: position arguments not expected", a.GetName())
		return 1
	}

	credential, err := cred.New(ctx, r.ts)
	if err != nil {
		log.Errorf("auth error: %v", err)
		return 1
	}

	msg := "Logged in"
	if credential.Email != "" {
		msg += fmt.Sprintf(" as %q", credential.Email)
	}
	msg += fmt.Sprintf(" by %q", credential.Type)
	fmt.Println(msg)

	r.reopt.UpdateProjectID(r.projectID)
	if r.reopt.IsValid() {
		client, err := reapi.New(ctx, credential, *r.reopt)
		fmt.Printf("Using REAPI instance %q via %q\n", r.reopt.Instance, r.reopt.Address)
		if err != nil {
			log.Errorf("REAPI access error: %v", err)
			return 1
		}
		defer client.Close()
	}

	return 0
}
