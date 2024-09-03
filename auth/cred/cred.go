// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package cred provides gRPC / API credentials to authenticate to network services.
package cred

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"os/exec"

	"golang.org/x/oauth2"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/oauth"

	"go.chromium.org/luci/auth"
	"go.chromium.org/luci/hardcoded/chromeinfra"

	"infra/build/siso/o11y/clog"
)

// Cred holds credentials and derived values.
type Cred struct {
	// Type is credential type. e.g. "luci-auth", "gcloud", etc.
	Type string

	// Email is authenticated email.
	Email string

	rpcCredentials credentials.PerRPCCredentials
	tokenSource    oauth2.TokenSource
}

type Options struct {
	LUCIAuth    auth.Options
	TokenSource oauth2.TokenSource
}

// AuthOpts returns the LUCI auth options that Siso uses.
func AuthOpts(credHelper string) Options {
	authOpts := chromeinfra.DefaultAuthOptions()
	authOpts.Scopes = []string{
		auth.OAuthScopeEmail,
		"https://www.googleapis.com/auth/cloud-platform",
	}
	var tokenSource oauth2.TokenSource
	if credHelper != "" {
		tokenSource = credHelperTokenSource{credHelper}
	} else {
		tokenSource = gcloudTokenSource{}
	}
	return Options{
		LUCIAuth:    authOpts,
		TokenSource: tokenSource,
	}
}

// New creates a Cred using LUCI auth's default options.
// It ensures that the user is logged in and returns an error otherwise.
func New(ctx context.Context, opts Options) (Cred, error) {
	t := "luci-auth"
	authenticator := auth.NewAuthenticator(ctx, auth.SilentLogin, opts.LUCIAuth)
	err := authenticator.CheckLoginRequired()
	if err != nil {
		// Check if the user is already logged in via `luci-auth login --scopes-context`.
		// If yes, we can use that token and avoid having to prompt for another login.
		authOpts := chromeinfra.DefaultAuthOptions()
		// same scope as `--scopes-context`
		// https://crrev.com/bdbc1802265493619ac518d392776af6593fd1e0/auth/client/authcli/authcli.go#22
		authOpts.Scopes = []string{
			"https://www.googleapis.com/auth/cloud-platform",
			"https://www.googleapis.com/auth/firebase",
			"https://www.googleapis.com/auth/gerritcodereview",
			"https://www.googleapis.com/auth/userinfo.email",
		}
		t = "luci-auth-context"
		authenticator = auth.NewAuthenticator(ctx, auth.SilentLogin, authOpts)
		err = authenticator.CheckLoginRequired()
	}
	if err != nil {
		if opts.TokenSource == nil {
			return Cred{}, err
		}
		tok, err := opts.TokenSource.Token()
		if err != nil {
			if errors.Is(err, exec.ErrNotFound) {
				return Cred{}, fmt.Errorf("need to run `siso login`, or install `gcloud` and `gcloud auth login --update-adc`: %w", err)
			}
			return Cred{}, err
		}
		t, _ := tok.Extra("x-token-source").(string)
		email := tok.Extra("x-token-email").(string)
		clog.Infof(ctx, "use auth %v email: %s", t, email)
		ts := oauth2.ReuseTokenSource(tok, opts.TokenSource)
		return Cred{
			Type:  t,
			Email: email,
			rpcCredentials: oauth.TokenSource{
				TokenSource: ts,
			},
			tokenSource: ts,
		}, nil
	}

	email, err := authenticator.GetEmail()
	if err != nil {
		return Cred{}, err
	}

	tokenSource, err := authenticator.TokenSource()
	if err != nil {
		return Cred{}, err
	}

	rpcCredentials, err := authenticator.PerRPCCredentials()
	if err != nil {
		return Cred{}, err
	}

	clog.Infof(ctx, "use luci-auth email: %s", email)
	return Cred{
		Type:           t,
		Email:          email,
		rpcCredentials: rpcCredentials,
		tokenSource:    tokenSource,
	}, nil
}

// GRPCDialOptions returns grpc's dial options to use the credential.
func (c Cred) GRPCDialOptions() []grpc.DialOption {
	if c.rpcCredentials == nil {
		return nil
	}
	return []grpc.DialOption{
		grpc.WithPerRPCCredentials(c.rpcCredentials),
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{})),
	}
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
