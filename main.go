// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Siso is a Ninja-compatible build system optimized for remote execution.
package main

import (
	"flag"
	"fmt"
	"net/http"
	_ "net/http/pprof" // import to let pprof register its HTTP handlers
	"os"
	"os/signal"
	"runtime"
	"runtime/pprof"
	"runtime/trace"

	"github.com/charmbracelet/log"
	"github.com/maruel/subcommands"

	"go.chromium.org/luci/auth/client/authcli"
	"go.chromium.org/luci/common/cli"
	"go.chromium.org/luci/common/system/signals"

	"go.chromium.org/infra/build/siso/auth/cred"
	"go.chromium.org/infra/build/siso/subcmd/authcheck"
	"go.chromium.org/infra/build/siso/subcmd/help"
	"go.chromium.org/infra/build/siso/subcmd/ninja"
	"go.chromium.org/infra/build/siso/subcmd/query"
	"go.chromium.org/infra/build/siso/subcmd/scandeps"
	"go.chromium.org/infra/build/siso/subcmd/version"
)

var (
	pprofAddr     string
	cpuprofile    string
	memprofile    string
	blockprofRate int
	mutexprofFrac int
	traceFile     string
)

const versionID = "v1.1.29"
const versionStr = "siso " + versionID

func getApplication(authOpts cred.Options) *cli.Application {
	return &cli.Application{
		Name:  "siso",
		Title: "Ninja-compatible build system optimized for remote execution",
		Commands: []*subcommands.Command{
			help.Cmd(),
			ninja.Cmd(authOpts, versionID),
			query.Cmd(),
			scandeps.Cmd(),
			authcheck.Cmd(authOpts),

			authcli.SubcommandLogin(authOpts.LUCIAuth, "login", true),
			authcli.SubcommandLogout(authOpts.LUCIAuth, "logout", true),
			version.Cmd(versionStr),
		},
		EnvVars: map[string]subcommands.EnvVarDefinition{
			"SISO_PROJECT": {
				ShortDesc: "cloud project ID",
			},
			"SISO_REAPI_INSTANCE": {
				Advanced:  true,
				ShortDesc: "RE API instance name",
				Default:   "default_instance",
			},
			"SISO_REAPI_ADDRESS": {
				Advanced:  true,
				ShortDesc: "RE API address",
				Default:   "remotebuildexecution.googleapis.com:443",
			},
			"SISO_CREDENTIAL_HELPER": {
				Advanced:  true,
				ShortDesc: "credential helper",
				Default:   "",
			},
		},
	}
}

func main() {
	// Wraps sisoMain() because os.Exit() doesn't wait defers.
	os.Exit(sisoMain())
}

func sisoMain() int {
	flag.Usage = func() {
		fmt.Fprint(flag.CommandLine.Output(), `
Usage: siso [command] [arguments]

Use "siso help" to display commands.
Use "siso help [command]" for more information about a command.
Use "siso help -advanced" to display all commands.

`)
		fmt.Fprintf(flag.CommandLine.Output(), "flags of %s:\n", os.Args[0])
		flag.PrintDefaults()
	}

	flag.StringVar(&pprofAddr, "pprof_addr", "", `listen address for "go tool pprof". e.g. "localhost:6060"`)
	flag.StringVar(&cpuprofile, "cpuprofile", "", "write cpu profile to this file")
	flag.StringVar(&memprofile, "memprofile", "", "write memory profile to this file")
	flag.IntVar(&blockprofRate, "blockprof_rate", 0, "block profile rate")
	flag.IntVar(&mutexprofFrac, "mutexprof_frac", 0, "mutex profile fraction")
	flag.StringVar(&traceFile, "trace", "", `go trace output for "go tool trace"`)

	log.SetTimeFormat("2006-01-02 15:04:05")
	log.SetReportCaller(true)

	credHelper := ""
	if h, ok := os.LookupEnv("SISO_CREDENTIAL_HELPER"); ok {
		credHelper = h
	}
	flag.StringVar(&credHelper, "credential_helper", credHelper, "path to a credential helper. see https://github.com/EngFlow/credential-helper-spec/blob/main/spec.md")

	var printVersion bool
	flag.BoolVar(&printVersion, "version", false, "print version")
	flag.Parse()

	// Print a stack trace when a panic occurs.
	defer func() {
		if r := recover(); r != nil {
			const size = 64 << 10
			buf := make([]byte, size)
			buf = buf[:runtime.Stack(buf, false)]
			log.Fatalf("panic: %v\n%s", r, buf)
		}
	}()

	authOpts := cred.AuthOpts(credHelper)
	if printVersion {
		a := getApplication(authOpts)
		c := version.Cmd(versionStr)
		r := c.CommandRun()
		return r.Run(a, nil, nil)
	}

	if blockprofRate > 0 {
		runtime.SetBlockProfileRate(blockprofRate)
	}
	if mutexprofFrac > 0 {
		runtime.SetMutexProfileFraction(mutexprofFrac)
	}

	// Start an HTTP server that can be used to profile Siso during runtime.
	if pprofAddr != "" {
		// https://pkg.go.dev/net/http/pprof
		fmt.Fprintf(os.Stderr, "pprof is enabled, listening at http://%s/debug/pprof/\n", pprofAddr)
		go func() {
			log.Infof("pprof http listener: %v", http.ListenAndServe(pprofAddr, nil))
		}()
		defer func() {
			fmt.Fprintf(os.Stderr, "pprof is still listening at http://%s/debug/pprof/\n", pprofAddr)
			fmt.Fprintln(os.Stderr, "Press Ctrl-C to terminate the process")
			sigch := make(chan os.Signal, 1)
			signal.Notify(sigch, signals.Interrupts()...)
			<-sigch
		}()
	}

	// Save a CPU profile to disk on exit.
	if cpuprofile != "" {
		f, err := os.Create(cpuprofile)
		if err != nil {
			log.Fatalf("failed to create cpuprofile file: %v", err)
		}
		err = pprof.StartCPUProfile(f)
		if err != nil {
			log.Errorf("failed to start CPU profiler: %v", err)
		}
		defer pprof.StopCPUProfile()
	}

	// Save a heap profile to disk on exit.
	if memprofile != "" {
		f, err := os.Create(memprofile)
		if err != nil {
			log.Fatalf("failed to create memprofile file: %v", err)
		}
		defer func() {
			err := pprof.WriteHeapProfile(f)
			if err != nil {
				log.Errorf("failed to write heap profile: %v", err)
			}
		}()
	}

	// Save a go trace to disk during execution.
	if traceFile != "" {
		fmt.Fprintf(os.Stderr, "enable go trace in %q\n", traceFile)
		f, err := os.Create(traceFile)
		if err != nil {
			log.Fatalf("Failed to create go trace output file: %v", err)
		}
		defer func() {
			fmt.Fprintf(os.Stderr, "go trace: go tool trace %s\n", traceFile)
			cerr := f.Close()
			if cerr != nil {
				log.Fatalf("Failed to close go trace output file: %v", cerr)
			}
		}()
		if err := trace.Start(f); err != nil {
			log.Fatalf("Failed to start go trace: %v", err)
		}
		defer trace.Stop()
	}

	return subcommands.Run(getApplication(authOpts), nil)
}
