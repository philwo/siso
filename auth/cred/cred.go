// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Cred provides gRPC / API credentials to authenticate to network services.
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
	authenticator *auth.Authenticator
	rpcCreds      credentials.PerRPCCredentials
	tokenSource   oauth2.TokenSource
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
	au := auth.NewAuthenticator(ctx, auth.SilentLogin, authOpts)
	if err := au.CheckLoginRequired(); err != nil {
		return Cred{}, err
	}

	ts, err := au.TokenSource()
	if err != nil {
		return Cred{}, err
	}

	rc, err := au.PerRPCCredentials()
	if err != nil {
		return Cred{}, err
	}

	return Cred{
		authenticator: au,
		rpcCreds:      rc,
		tokenSource:   ts,
	}, nil
}

// GRPCDialOptions returns grpc's dial options to use the credential.
func (c Cred) GRPCDialOptions() []grpc.DialOption {
	if c.rpcCreds == nil {
		return nil
	}
	return []grpc.DialOption{
		grpc.WithPerRPCCredentials(c.rpcCreds),
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
