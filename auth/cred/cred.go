// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package cred provides gRPC / API credentials to authenticate to network services.
package cred

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/charmbracelet/log"
	"golang.org/x/oauth2"
	"google.golang.org/api/option"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/oauth"
)

// Cred holds credentials and derived values.
type Cred struct {
	// Type is credential type. e.g. "luci-auth", etc.
	Type string

	// Email is authenticated email.
	Email string

	rpcCredentials credentials.PerRPCCredentials
	tokenSource    oauth2.TokenSource
}

// AuthOpts returns the LUCI auth options that Siso uses.
func AuthOpts(credHelper string) oauth2.TokenSource {
	// If a credential helper is specified, use it.
	if credHelper != "" {
		return credHelperTokenSource{
			credHelper: credHelper,
			args:       []string{"get"},
		}
	}

	// Use the default LUCI auth credential helper.
	credHelper, err := exec.LookPath("luci-auth")
	if err != nil {
		log.Warnf("luci-auth not found in PATH: %v", err)
		return nil
	}

	return credHelperTokenSource{
		credHelper: credHelper,
		args: []string{
			"token",
			"-scopes-context",
			"-json-output=-",
			"-json-format=bazel",
			"-lifetime=5m",
		},
	}
}

// New creates a Cred using LUCI auth's default options.
// It ensures that the user is logged in and returns an error otherwise.
func New(ctx context.Context, ts oauth2.TokenSource) (Cred, error) {
	tok, err := ts.Token()
	if err != nil {
		if ctx.Err() != nil {
			return Cred{}, err
		}
		return Cred{}, fmt.Errorf("need to run `siso login`: %w", err)
	}

	// Get the token type and email from the token.
	t, _ := tok.Extra("x-token-source").(string)
	email, _ := tok.Extra("x-token-email").(string)
	log.Infof("Logged in as %q by %q", email, t)

	// Reuse a valid token as long as it is not expired.
	ts = oauth2.ReuseTokenSource(tok, ts)

	return Cred{
		Type:  t,
		Email: email,
		rpcCredentials: oauth.TokenSource{
			TokenSource: ts,
		},
		tokenSource: ts,
	}, nil
}

// ClientOptions returns googleapi's client options to use the credential.
func (c Cred) ClientOptions() []option.ClientOption {
	if c.tokenSource == nil {
		return nil
	}
	return []option.ClientOption{
		option.WithTokenSource(c.tokenSource),
	}
}
