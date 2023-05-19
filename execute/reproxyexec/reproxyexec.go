// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package reproxyexec executes cmd with reproxy.
package reproxyexec

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	cpb "github.com/bazelbuild/remote-apis-sdks/go/api/command"
	"github.com/bazelbuild/remote-apis-sdks/go/pkg/command"
	"github.com/bazelbuild/remote-apis-sdks/go/pkg/retry"
	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	log "github.com/golang/glog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"infra/build/siso/execute"
	"infra/build/siso/o11y/clog"
	"infra/build/siso/o11y/trace"
	"infra/build/siso/reapi/digest"
	ppb "infra/third_party/reclient/api/proxy"
)

const (
	// dialTimeout defines the timeout we'd like to use to dial reproxy.
	dialTimeout = 3 * time.Minute
	// grpcMaxMsgSize is the max value of gRPC response that can be received by the client (in bytes).
	grpcMaxMsgSize = 1024 * 1024 * 32 // 32MB (default is 4MB)
	// wrapperOverheadKey is the key for the wrapper overhead metric passed to the proxy.
	wrapperOverheadKey = "WrapperOverhead"
)

var (
	backoff     = retry.ExponentialBackoff(1*time.Second, 15*time.Second, retry.Attempts(10))
	shouldRetry = func(err error) bool {
		if err == context.DeadlineExceeded {
			return true
		}
		s, ok := status.FromError(err)
		if !ok {
			return false
		}
		switch s.Code() {
		case codes.Canceled, codes.Unknown, codes.DeadlineExceeded, codes.Aborted,
			codes.Unavailable, codes.ResourceExhausted:
			return true
		default:
			return false
		}
	}
)

// ReproxyExec is executor with reproxy.
type ReproxyExec struct {
	serverAddr string
}

// New creates new remote executor.
func New(ctx context.Context, serverAddr string) *ReproxyExec {
	return &ReproxyExec{
		serverAddr: serverAddr,
	}
}

// Run runs a cmd.
func (re *ReproxyExec) Run(ctx context.Context, cmd *execute.Cmd) error {
	ctx, span := trace.NewSpan(ctx, "reproxy-exec")
	defer span.Close(nil)

	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	// Create gRPC connection with the provided address.
	// TODO(b/273407069): this will be problematic on windows, reuse connections instead.
	conn, err := DialContext(ctx, re.serverAddr)
	if err != nil {
		log.Fatalf("Fail to dial %s: %v", re.serverAddr, err)
	}
	defer conn.Close()

	// Create reproxy client over this connection.
	proxy := ppb.NewCommandsClient(conn)

	resp, err := runCommand(ctx, proxy, cmd)
	err = re.processResponse(ctx, cmd, resp, err)
	if err != nil {
		log.Errorf("Command failed for cmd \"%s\": %v", cmd.Desc, err)
	}
	return err
}

// Proxy is the interface of the RE Proxy API.
type Proxy interface {
	RunCommand(context.Context, *ppb.RunRequest, ...grpc.CallOption) (*ppb.RunResponse, error)
}

// runCommand runs a command through the RE proxy.
func runCommand(ctx context.Context, proxy Proxy, cmd *execute.Cmd) (*ppb.RunResponse, error) {
	req, err := createRequest(cmd)
	if err != nil {
		return nil, err
	}
	var resp *ppb.RunResponse
	err = retry.WithPolicy(ctx, shouldRetry, backoff, func() error {
		resp, err = proxy.RunCommand(ctx, req)
		return err
	})
	return resp, err
}

func createRequest(cmd *execute.Cmd) (*ppb.RunRequest, error) {
	// TODO(b/273407069): not everything is a tool. use the current label for input from rewrapper cfg or elsewhere.
	labels := map[string]string{"type": "tool"}
	c := &cpb.Command{
		Identifiers: &cpb.Identifiers{
			CommandId: cmd.ID,
		},
		ExecRoot: cmd.ExecRoot,
		Input: &cpb.InputSpec{
			Inputs: cmd.Inputs,
		},
		Output: &cpb.OutputSpec{
			OutputFiles: cmd.Outputs,
		},
		Args:             cmd.Args,
		ExecutionTimeout: int32(cmd.Timeout.Seconds()),
		WorkingDirectory: cmd.Dir,
		Platform:         cmd.Platform,
	}
	strategy := ppb.ExecutionStrategy_UNSPECIFIED
	// TODO(b/273407069): temporarily hardcoded to remote. instead get execution strategy from rewrapper cfg or elsewhere.
	if res, ok := ppb.ExecutionStrategy_Value_value[strings.ToUpper("remote")]; ok {
		strategy = ppb.ExecutionStrategy_Value(res)
	}
	md := &ppb.Metadata{EventTimes: map[string]*cpb.TimeInterval{
		wrapperOverheadKey: {From: command.TimeToProto(time.Now())},
	}}
	md.Environment = os.Environ()
	return &ppb.RunRequest{
		Command: c,
		Labels:  labels,
		ExecutionOptions: &ppb.ProxyExecutionOptions{
			ExecutionStrategy: strategy,
			CompareWithLocal:  false,
			NumLocalReruns:    0,
			NumRemoteReruns:   0,
			RemoteExecutionOptions: &ppb.RemoteExecutionOptions{
				AcceptCached: !cmd.SkipCacheLookup,
				DoNotCache:   cmd.DoNotCache,
				// TODO(b/273407069): make this configurable, use siso download strategy config?
				DownloadOutputs:              true,
				Wrapper:                      cmd.RemoteWrapper,
				CanonicalizeWorkingDir:       cmd.CanonicalizeDir,
				PreserveUnchangedOutputMtime: false,
			},
			LogEnvironment: false,
			// Necessary for metadata such as digests to be returned.
			IncludeActionLog: true,
		},
		ToolchainInputs: cmd.ToolInputs,
		Metadata:        md,
	}, nil
}

func (re *ReproxyExec) processResponse(ctx context.Context, cmd *execute.Cmd, response *ppb.RunResponse, err error) error {
	if response == nil {
		return errors.New("no response")
	}

	cached := response.GetResult().GetStatus() == cpb.CommandResultStatus_CACHE_HIT
	if response.GetResult().GetExitCode() == 0 && err == nil {
		clog.Infof(ctx, "exit=%d cache=%t", response.GetResult().GetExitCode(), cached)
	} else {
		clog.Warningf(ctx, "exit=%d cache=%t response=%v err:%v", response.GetResult().GetExitCode(), cached, response, err)
	}

	// TODO(b/273407069): this is nowhere near a complete ActionResult. LogRecord has lots of info, add that info.
	result := &rpb.ActionResult{
		ExitCode:  response.GetResult().GetExitCode(),
		StdoutRaw: response.GetStdout(),
		StderrRaw: response.GetStderr(),
		// TODO(b/273407069): this is nowhere near a complete ExecutedActionMetadata. add extra info where siso needs it.
		ExecutionMetadata: &rpb.ExecutedActionMetadata{
			Worker: response.GetExecutionId(),
		},
	}

	// for now, update hashfs as if this was local exec.
	// https://source.chromium.org/chromium/infra/infra/+/main:go/src/infra/build/siso/execute/localexec/localexec.go;l=58-76;drc=f9a8d895ee9b5d51a5d5da0131de75c021b4a212
	// TODO(b/273407069): use the hashes returned by reproxy.
	if cmd.HashFS == nil {
		return nil
	}
	entries, err := cmd.HashFS.LocalEntries(ctx, cmd.ExecRoot, cmd.AllOutputs())
	if err != nil {
		return err
	}
	err = cmd.HashFS.Update(ctx, cmd.ExecRoot, entries, time.Now(), cmd.CmdHash, digest.Digest{})
	if err != nil {
		return err
	}
	if cmd.Depfile != "" {
		cmd.HashFS.Forget(ctx, cmd.ExecRoot, []string{cmd.Depfile})
	}

	// any stdout/stderr is unexpected, write this out and stop if received.
	if len(response.Stdout) > 0 {
		cmd.StdoutWriter().Write(response.Stdout)
	}
	if len(response.Stderr) > 0 {
		cmd.StderrWriter().Write(response.Stderr)
	}
	err = resultErr(response)
	if err != nil {
		return err
	}

	cmd.SetActionResult(result)
	return nil
}

func resultErr(response *ppb.RunResponse) error {
	if response.GetResult() == nil {
		return errors.New("no result")
	}
	if response.GetResult().ExitCode == 0 {
		return nil
	}
	return execute.ExitError{
		ExitCode: int(response.GetResult().GetExitCode()),
	}
}
