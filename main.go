// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Siso is a Ninja-compatible build system optimized for remote execution.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	_ "net/http/pprof" // import to let pprof register its HTTP handlers
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/charmbracelet/log"

	"go.chromium.org/infra/build/siso/auth/cred"
	"go.chromium.org/infra/build/siso/hashfs"
	"go.chromium.org/infra/build/siso/reapi"
	"go.chromium.org/infra/build/siso/subcmd/ninja"
)

var (
	pprofAddr     string
	blockprofRate int
	mutexprofFrac int
)

const versionID = "v1.1.29"
const versionStr = "siso " + versionID

type errInterrupted struct{}

func (errInterrupted) Error() string        { return "interrupt by signal" }
func (errInterrupted) Is(target error) bool { return target == context.Canceled }

// HandleInterrupt calls 'fn' in a separate goroutine on SIGTERM or Ctrl+C.
//
// When SIGTERM or Ctrl+C comes for a second time, logs to stderr and kills
// the process immediately via os.Exit(1).
//
// Returns a callback that can be used to remove the installed signal handlers.
func HandleInterrupt(fn func()) (stopper func()) {
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)

	go func() {
		handled := false
		for range ch {
			if handled {
				fmt.Fprintf(os.Stderr, "Got second interrupt signal. Aborting.\n")
				os.Exit(1)
			}
			handled = true
			go fn()
		}
	}()

	return func() {
		signal.Stop(ch)
		close(ch)
	}
}

func main() {
	// Wraps sisoMain() because os.Exit() doesn't wait defers.
	os.Exit(sisoMain(context.Background()))
}

func sisoMain(ctx context.Context) int {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer HandleInterrupt(func() {
		cancel(errInterrupted{})
	})()

	c := &ninja.NinjaOpts{}

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
	flag.IntVar(&blockprofRate, "blockprof_rate", 0, "block profile rate")
	flag.IntVar(&mutexprofFrac, "mutexprof_frac", 0, "mutex profile fraction")

	flag.StringVar(&c.Dir, "C", ".", "ninja running directory")
	flag.StringVar(&c.ConfigName, "config", "", "config name passed to starlark")
	flag.StringVar(&c.ProjectID, "project", os.Getenv("SISO_PROJECT"), "cloud project ID. can set by $SISO_PROJECT")

	flag.BoolVar(&c.Offline, "offline", false, "offline mode.")
	flag.BoolVar(&c.Offline, "o", false, "alias of `-offline`")
	if f := flag.Lookup("offline"); f != nil {
		if s := os.Getenv("RBE_remote_disabled"); s != "" {
			err := f.Value.Set(s)
			if err != nil {
				log.Errorf("invalid RBE_remote_disabled=%q: %v", s, err)
			}
		}
	}
	flag.BoolVar(&c.DryRun, "n", false, "dry run")
	flag.BoolVar(&c.Clobber, "clobber", false, "clobber build")
	flag.StringVar(&c.ActionSalt, "action_salt", "", "action salt")

	flag.IntVar(&c.RemoteJobs, "remote_jobs", 0, "run N remote jobs in parallel. when the value is <= 0, it will be computed based on # of CPUs.")
	flag.StringVar(&c.Fname, "f", "build.ninja", "input build manifest filename (relative to -C)")

	flag.StringVar(&c.ConfigRepoDir, "config_repo_dir", "build/config/siso", "config repo directory (relative to exec root)")
	flag.StringVar(&c.ConfigFilename, "load", "@config//main.star", "config filename (@config// is --config_repo_dir)")
	flag.StringVar(&c.OutputLocalStrategy, "output_local_strategy", "full", `strategy for output_local. "full": download all outputs. "greedy": downloads most outputs except intermediate objs. "minimum": downloads as few as possible`)
	flag.StringVar(&c.DepsLogFile, "deps_log", ".siso_deps", "deps log filename (relative to -C)")

	c.Fsopt = new(hashfs.Option)
	c.Fsopt.StateFile = ".siso_fs_state"

	c.Reopt = new(reapi.Option)

	addr := os.Getenv("SISO_REAPI_ADDRESS")
	if addr == "" {
		addr = "remotebuildexecution.googleapis.com:443"
	}
	flag.StringVar(&c.Reopt.Address, "reapi_address", addr, "reapi address")
	flag.StringVar(&c.Reopt.CASAddress, "reapi_cas_address", "", "reapi cas address (if empty, share conn with "+"reapi_address)")
	instance := os.Getenv("SISO_REAPI_INSTANCE")
	if instance == "" {
		instance = "default_instance"
	}
	flag.StringVar(&c.Reopt.Instance, "reapi_instance", instance, "reapi instance name")

	flag.BoolVar(&c.Reopt.Insecure, "reapi_insecure", os.Getenv("RBE_service_no_security") == "true", "reapi insecure mode. default can be set by $RBE_service_no_security")

	flag.StringVar(&c.Reopt.TLSClientAuthCert, "reapi_tls_client_auth_cert", os.Getenv("RBE_tls_client_auth_cert"), "Certificate to use when using mTLS to connect to the RE api service. default can be set by $RBE_tls_client_auth_cert")
	flag.StringVar(&c.Reopt.TLSClientAuthKey, "reapi_tls_client_auth_key", os.Getenv("RBE_tls_client_auth_key"), "Key to use when using mTLS to connect to the RE api service. default can be set by $RBE_tls_client_auth_key")

	flag.Int64Var(&c.Reopt.CompressedBlob, "reapi_compress_blob", 1024, "use compressed blobs if server supports compressed blobs and size is bigger than this. specify 0 to disable comporession.")

	// https://grpc.io/docs/guides/keepalive/#keepalive-configuration-specification
	// b/286237547 - RBE suggests 30s
	flag.DurationVar(&c.Reopt.KeepAliveParams.Time, "reapi_grpc_keepalive_time", 30*time.Second, "grpc keepalive time")
	flag.DurationVar(&c.Reopt.KeepAliveParams.Timeout, "reapi_grpc_keepalive_timeout", 20*time.Second, "grpc keepalive timeout")
	flag.BoolVar(&c.Reopt.KeepAliveParams.PermitWithoutStream, "reapi_grpc_keepalive_permit_without_stream", false, "grpc keepalive permit without stream")

	flag.BoolVar(&c.ReCacheEnableRead, "re_cache_enable_read", true, "remote exec cache enable read")

	log.SetTimeFormat("2006-01-02 15:04:05")
	log.SetReportCaller(true)

	flag.StringVar(&c.CredHelper, "credential_helper", os.Getenv("SISO_CREDENTIAL_HELPER"), "path to a credential helper. see https://github.com/EngFlow/credential-helper-spec/blob/main/spec.md")

	var printVersion bool
	flag.BoolVar(&printVersion, "version", false, "print version")
	flag.Parse()

	c.Targets = flag.Args()

	// Print a stack trace when a panic occurs.
	defer func() {
		if r := recover(); r != nil {
			const size = 64 << 10
			buf := make([]byte, size)
			buf = buf[:runtime.Stack(buf, false)]
			log.Fatalf("panic: %v\n%s", r, buf)
		}
	}()

	c.Ts = cred.AuthOpts(c.CredHelper)
	if printVersion {
		fmt.Fprintf(os.Stderr, "%s\n", versionStr)
		return 0
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
			signal.Notify(sigch, os.Interrupt, syscall.SIGTERM)
			<-sigch
		}()
	}

	return c.Run(ctx)
}
