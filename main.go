// Copyright 2023 The Chromium Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"strings"

	log "github.com/golang/glog"
	"go.chromium.org/luci/auth"
	"go.chromium.org/luci/common/system/signals"
	"go.chromium.org/luci/hardcoded/chromeinfra"
	"google.golang.org/grpc/credentials"
)

// Siso is an experimental build tool.

func main() {
	flag.Usage = func() {
		out := flag.CommandLine.Output()
		fmt.Fprintf(out, "Usage of %s:\n", os.Args[0])
		fmt.Fprintf(out, "global flags:\n")
		flag.PrintDefaults()
	}

	flag.Parse()
	if flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}
	ctx := context.Background()
	var err error

	// Use luci-auth to authenticate.
	// TODO(b/267435657): Add auth subcommands to be able to authenticate without luci-auth.
	// This is blocked on b/246687010 to use github.com/maruel/subcommand.
	authOpts := chromeinfra.DefaultAuthOptions()
	authOpts.Scopes = []string{auth.OAuthScopeEmail, "https://www.googleapis.com/auth/cloud-platform"}
	a := auth.NewAuthenticator(ctx, auth.SilentLogin, authOpts)
	cred, err := a.PerRPCCredentials()
	if err != nil {
		fmt.Printf("Failed to authenticate. Please login with `luci-auth login -scopes=\"%s\"`.\n", strings.Join(authOpts.Scopes, " "))
		os.Exit(1)
	}

	err = sisoMain(ctx, flag.Args(), cred)

	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		flag.Usage()
		os.Exit(2)
	}
	if err != nil {
		log.Exitf("Error: %v", err)
	}
}

func sisoMain(ctx context.Context, args []string, cred credentials.PerRPCCredentials) error {
	ctx, cancel := context.WithCancel(ctx)
	defer signals.HandleInterrupt(cancel)()

	// Flush the log on exit to not lose any messages.
	defer log.Flush()

	// Print a stack trace when a panic occurs.
	defer func() {
		if r := recover(); r != nil {
			const size = 64 << 10
			buf := make([]byte, size)
			buf = buf[:runtime.Stack(buf, false)]
			log.Fatalf("panic: %v\n%s", r, buf)
		}
	}()

	// Print build information to the log.
	buildinfo, ok := debug.ReadBuildInfo()
	log.Infof("buildinfo: path=%q ok=%t", buildinfo.Path, ok)
	if ok {
		log.Infof("main module: %s %s", moduleInfo(&buildinfo.Main), vcsInfo(buildinfo))
		if log.V(1) {
			for _, m := range buildinfo.Deps {
				log.Infof("deps module: %s", moduleInfo(m))
			}
			for _, bs := range buildinfo.Settings {
				log.Infof("build %s=%s", bs.Key, bs.Value)
			}
		}
	}

	var err error
	// TODO(b/246687010) use subcommands library
	switch args[0] {
	default:
		err = fmt.Errorf("unknown subcommand %q: %w", args[0], flag.ErrHelp)
	}
	return err
}

func moduleInfo(m *debug.Module) string {
	if m == nil {
		return "<nil>"
	}
	return fmt.Sprintf("path:%s version:%s sum:%s replace:%s", m.Path, m.Version, m.Sum, moduleInfo(m.Replace))
}

func vcsInfo(buildinfo *debug.BuildInfo) string {
	m := make(map[string]string)
	for _, bs := range buildinfo.Settings {
		if strings.HasPrefix(bs.Key, "vcs.") {
			m[bs.Key] = bs.Value
		}
	}
	return fmt.Sprintf("vcs[revision=%s time=%s modified=%s]", m["vcs.revision"], m["vcs.time"], m["vcs.modified"])
}
