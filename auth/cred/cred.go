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
	"os"

	"github.com/golang/glog"
	"golang.org/x/oauth2"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/oauth"

	"go.chromium.org/luci/auth"
	"go.chromium.org/luci/hardcoded/chromeinfra"
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

// Options is an options for credentials.
type Options struct {
	LUCIAuth         auth.Options
	FallbackLUCIAuth auth.Options

	PerRPCCredentials credentials.PerRPCCredentials
	// TokenSource is used when PerRPCCredentials is not set.
	TokenSource oauth2.TokenSource
}

// AuthOpts returns the LUCI auth options that Siso uses.
func AuthOpts(credHelperPath string) Options {
	var authOpts, fallbackAuthOpts auth.Options
	// don't use luci-auth if $HOME is not set,
	// since chromeinfra.DefaultAuthOptions will print
	// "Can't resolve $HOME: $HOME is not defined"
	// to stderr.
	// https://crrev.com/d0549b3d2cf7923dd33d1810ac5a8348d09237ac/hardcoded/chromeinfra/chromeinfra.go#137
	_, err := os.UserHomeDir()
	if err == nil {
		authOpts = chromeinfra.DefaultAuthOptions()
		authOpts.Scopes = []string{
			auth.OAuthScopeEmail,
			"https://www.googleapis.com/auth/cloud-platform",
		}
		// If the user is already logged in via `luci-auth login --scopes-context`,
		// we can use that token and avoid having to prompt for another login.
		fallbackAuthOpts = chromeinfra.DefaultAuthOptions()
		// same scope as `--scopes-context`
		// https://crrev.com/bdbc1802265493619ac518d392776af6593fd1e0/auth/client/authcli/authcli.go#22
		fallbackAuthOpts.Scopes = []string{
			"https://www.googleapis.com/auth/cloud-platform",
			"https://www.googleapis.com/auth/firebase",
			"https://www.googleapis.com/auth/gerritcodereview",
			"https://www.googleapis.com/auth/userinfo.email",
		}
	}
	var perRPCCredentials credentials.PerRPCCredentials
	var tokenSource oauth2.TokenSource
	if credHelperPath != "" {
		h := &credHelper{path: credHelperPath}
		perRPCCredentials = h
		tokenSource = &credHelperGoogle{h: h}
	}
	return Options{
		LUCIAuth:          authOpts,
		FallbackLUCIAuth:  fallbackAuthOpts,
		PerRPCCredentials: perRPCCredentials,
		TokenSource:       tokenSource,
	}
}

// New creates a Cred using LUCI auth's default options.
// It ensures that the user is logged in and returns an error otherwise.
func New(ctx context.Context, opts Options) (Cred, error) {
	var t string
	var authenticator *auth.Authenticator
	_, err := os.UserHomeDir()
	if err != nil {
		glog.Warningf("disable luci-auth: %v", err)
		err = fmt.Errorf("disable luci-auth: %w", err)
	} else {
		t = "luci-auth-cloud-platform"
		authenticator = auth.NewAuthenticator(ctx, auth.SilentLogin, opts.LUCIAuth)
		err = authenticator.CheckLoginRequired()
		if err != nil && len(opts.FallbackLUCIAuth.Scopes) > 0 {
			t = "luci-auth-context"
			authenticator = auth.NewAuthenticator(ctx, auth.SilentLogin, opts.FallbackLUCIAuth)
			err = authenticator.CheckLoginRequired()
		}
	}
	if err != nil {
		if opts.TokenSource == nil {
			return Cred{}, err
		}
		var email string
		ts := opts.TokenSource
		tok, err := ts.Token()
		if err != nil {
			if ctx.Err() != nil {
				return Cred{}, err
			}
			if errors.Is(err, errNoAuthorization) {
				if ch, ok := ts.(*credHelperGoogle); ok {
					t = ch.h.path
					glog.Warningf("use auth %s, no token source %v", ch.h.path, err)
				} else {
					t = fmt.Sprintf("%T", ts)
					glog.Warningf("use auth %T, no token source: %v", ts, err)
				}
				ts = nil
			} else {
				return Cred{}, fmt.Errorf("need to run `siso login`: %w", err)
			}
		} else {
			t, _ = tok.Extra("x-token-source").(string)
			email, _ = tok.Extra("x-token-email").(string)
			glog.Infof("use auth %v email: %s", t, email)
			ts = oauth2.ReuseTokenSource(tok, ts)
		}
		perRPCCredentials := opts.PerRPCCredentials
		if perRPCCredentials == nil {
			perRPCCredentials = oauth.TokenSource{
				TokenSource: ts,
			}
		}
		return Cred{
			Type:              t,
			Email:             email,
			perRPCCredentials: perRPCCredentials,
			tokenSource:       ts,
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

	glog.Infof("use %s email: %s", t, email)
	return Cred{
		Type:              t,
		Email:             email,
		perRPCCredentials: rpcCredentials,
		tokenSource:       tokenSource,
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
