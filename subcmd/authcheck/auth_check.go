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

	"go.chromium.org/luci/common/cli"

	"go.chromium.org/infra/build/siso/auth/cred"
	"go.chromium.org/infra/build/siso/reapi"
)

func Cmd(authOpts cred.Options) *subcommands.Command {
	return &subcommands.Command{
		UsageLine: "auth-check",
		ShortDesc: "prints current auth status.",
		LongDesc:  "Prints current auth status.",
		CommandRun: func() subcommands.CommandRun {
			r := &authCheckRun{authOpts: authOpts}
			r.init()
			return r
		},
	}
}

type authCheckRun struct {
	subcommands.CommandRunBase
	authOpts  cred.Options
	projectID string
	reopt     *reapi.Option
}

func (r *authCheckRun) init() {
	r.Flags.StringVar(&r.projectID, "project", os.Getenv("SISO_PROJECT"), "cloud project ID. can set by $SISO_PROJECT")

	r.reopt = new(reapi.Option)
	envs := map[string]string{
		"SISO_REAPI_INSTANCE": os.Getenv("SISO_REAPI_INSTANCE"),
		"SISO_REAPI_ADDRESS":  os.Getenv("SISO_REAPI_ADDRESS"),
	}
	r.reopt.RegisterFlags(&r.Flags, envs)
}

func (r *authCheckRun) Run(a subcommands.Application, args []string, env subcommands.Env) int {
	ctx := cli.GetContext(a, r, env)
	if len(args) != 0 {
		fmt.Fprintf(a.GetErr(), "%s: position arguments not expected\n", a.GetName())
		return 1
	}
	credential, err := cred.New(ctx, r.authOpts)
	if err != nil {
		log.Errorf("auth error: %v", err)
		return 1
	}
	msg := fmt.Sprintf("Logged in by %s\n", credential.Type)
	if credential.Email != "" {
		msg += fmt.Sprintf(" as %s", credential.Email)
	}
	log.Info(msg)
	r.reopt.UpdateProjectID(r.projectID)
	if r.reopt.IsValid() {
		client, err := reapi.New(ctx, credential, *r.reopt)
		log.Infof("use reapi instance %s", r.reopt.Instance)
		if err != nil {
			log.Errorf("access error: %v", err)
			return 1
		}
		defer client.Close()
	}
	return 0
}
