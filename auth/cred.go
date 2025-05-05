// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package auth provides gRPC / API credentials to authenticate to network services.
package auth

import (
	"context"
	"crypto/tls"
	"fmt"

	"github.com/charmbracelet/log"
	"golang.org/x/oauth2"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/oauth"
)

// Cred holds credentials and derived values.
type Cred struct {
	// Type is credential type. e.g. "luci-auth", etc.
	Type string

	// Email is authenticated email.
	Email string

	perRPCCredentials credentials.PerRPCCredentials
	tokenSource       oauth2.TokenSource
}

// NewCred creates a Cred using LUCI auth's default options.
// It ensures that the user is logged in and returns an error otherwise.
func NewCred(ctx context.Context, ts oauth2.TokenSource) (Cred, error) {
	tok, err := ts.Token()
	if err != nil {
		if ctx.Err() != nil {
			return Cred{}, err
		}
		return Cred{}, fmt.Errorf("need to run `luci-auth login -scopes-context`: %w", err)
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

// grpcDialOptions returns grpc's dial options to use the credential.
func (c Cred) grpcDialOptions() []grpc.DialOption {
	perRPCCredentials := c.perRPCCredentials
	if perRPCCredentials == nil {
		return nil
	}
	return []grpc.DialOption{
		grpc.WithPerRPCCredentials(perRPCCredentials),
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{})),
	}
}

// ClientOptions returns client options to use the credential.
func (c Cred) ClientOptions() []option.ClientOption {
	dopts := c.grpcDialOptions()
	if len(dopts) > 0 {
		copts := []option.ClientOption{
			// disable Google Application Default, and use PerRPCCredentials in dial option.
			// https://github.com/googleapis/google-api-go-client/issues/3149
			option.WithoutAuthentication(),
		}
		for _, opt := range dopts {
			copts = append(copts, option.WithGRPCDialOption(opt))
		}
		return copts
	}
	if c.tokenSource == nil {
		return nil
	}
	return []option.ClientOption{
		option.WithTokenSource(c.tokenSource),
	}
}
