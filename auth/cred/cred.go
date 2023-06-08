// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package cred provides gRPC / API credentials to authenticate to network services.
package cred

import (
	"context"
	"crypto/tls"

	"go.chromium.org/luci/auth"
	"go.chromium.org/luci/hardcoded/chromeinfra"
	"golang.org/x/oauth2"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// Cred holds credentials and derived values.
type Cred struct {
	authenticator  *auth.Authenticator
	rpcCredentials credentials.PerRPCCredentials
	tokenSource    oauth2.TokenSource
}

// AuthOpts returns the LUCI auth options that Siso uses.
func AuthOpts() auth.Options {
	authOpts := chromeinfra.DefaultAuthOptions()
	authOpts.Scopes = []string{
		auth.OAuthScopeEmail,
		"https://www.googleapis.com/auth/cloud-platform",
	}
	return authOpts
}

// New creates a Cred using LUCI auth's default options.
// It ensures that the user is logged in and returns an error otherwise.
func New(ctx context.Context, authOpts auth.Options) (Cred, error) {
	authenticator := auth.NewAuthenticator(ctx, auth.SilentLogin, authOpts)
	if err := authenticator.CheckLoginRequired(); err != nil {
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

	return Cred{
		authenticator:  authenticator,
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
