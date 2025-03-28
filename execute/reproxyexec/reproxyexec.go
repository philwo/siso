// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package reproxyexec executes cmd with reproxy.
package reproxyexec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	cpb "github.com/bazelbuild/remote-apis-sdks/go/api/command"
	"github.com/bazelbuild/remote-apis-sdks/go/pkg/command"
	"github.com/bazelbuild/remote-apis-sdks/go/pkg/retry"
	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
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
	// defaultExecTimeout defines the default timeout to use for executions.
	defaultExecTimeout = time.Hour
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

// REProxyExec is executor with reproxy.
// Users of REProxyExec should ensure Close is called to clean up the connection.
type REProxyExec struct {
	conn        *grpc.ClientConn
	connErr     error
	connAddress string
	connOnce    sync.Once
}

// New creates new remote executor.
func New(ctx context.Context, addr string) *REProxyExec {
	return &REProxyExec{
		connAddress: addr,
	}
}

// Close cleans up the executor.
func (re *REProxyExec) Close() error {
	if re.conn == nil {
		return nil
	}
	return re.conn.Close()
}

// Enabled returns whether reproxy is enabled or not.
func (re *REProxyExec) Enabled() bool {
	return re.connAddress != ""
}

// Run runs a cmd.
func (re *REProxyExec) Run(ctx context.Context, cmd *execute.Cmd) error {
	ctx, span := trace.NewSpan(ctx, "reproxy-exec")
	defer span.Close(nil)

	// ignore cmd.REProxyConfig.ServerAddress, which is
	// default value in rewrapper.cfg, but will be overridden
	// by RBE_server_address (which we set in re.connAddress).
	if !re.Enabled() {
		return fmt.Errorf("reproxy mode is not enabled")
	}
	if len(cmd.REProxyConfig.Labels) == 0 {
		return fmt.Errorf("REProxy config has no labels")
	}
	if len(cmd.REProxyConfig.Platform) == 0 {
		return fmt.Errorf("REProxy config has no platform")
	}
	execTimeout := defaultExecTimeout
	if cmd.REProxyConfig.ExecTimeout != "" {
		parsed, err := time.ParseDuration(cmd.REProxyConfig.ExecTimeout)
		if err != nil {
			return fmt.Errorf("failed to parse timeout %q: %w", cmd.REProxyConfig.ExecTimeout, err)
		}
		if parsed == 0 {
			return fmt.Errorf("0 is not a valid REProxy timeout")
		}
		execTimeout = parsed
	}

	// Dial reproxy if no connection, ensure same address is used for subsequent calls.
	// Only one dial is allowed, if it fails all subsequent calls will fail fast.
	re.connOnce.Do(func() {
		ctx, cancel := context.WithTimeout(ctx, dialTimeout)
		defer cancel()
		re.conn, re.connErr = DialContext(ctx, re.connAddress)
	})
	if re.connErr != nil {
		return fmt.Errorf("fail to dial %s: %w", re.connAddress, re.connErr)
	}

	// Create REProxy client and send the request with backoff configuration above.
	// (No timeout applied due to use of backoff with maximum attempts allowed.)
	proxy := ppb.NewCommandsClient(re.conn)
	req, err := createRequest(ctx, cmd, execTimeout)
	if err != nil {
		return err
	}
	var resp *ppb.RunResponse
	err = retry.WithPolicy(ctx, shouldRetry, backoff, func() error {
		resp, err = proxy.RunCommand(ctx, req)
		return err
	})

	err = processResponse(ctx, cmd, resp, err)
	if err != nil {
		clog.Warningf(ctx, "Command failed for cmd %q: %v", cmd.Desc, err)
	}
	return err
}

func createRequest(ctx context.Context, cmd *execute.Cmd, execTimeout time.Duration) (*ppb.RunRequest, error) {
	c := &cpb.Command{
		Identifiers: &cpb.Identifiers{
			CommandId: cmd.ID,
		},
		ExecRoot: cmd.ExecRoot,
		Input: &cpb.InputSpec{
			Inputs: append(cmd.REProxyConfig.Inputs, cmd.AllInputs()...),
		},
		Output: &cpb.OutputSpec{
			OutputFiles: cmd.AllOutputs(),
		},
		Args:             cmd.Args,
		ExecutionTimeout: int32(execTimeout.Seconds()),
		WorkingDirectory: cmd.Dir,
		Platform:         cmd.REProxyConfig.Platform,
	}

	// Use exec strategy if found, otherwise fallback to unspecified.
	strategy := ppb.ExecutionStrategy_UNSPECIFIED
	if res, ok := ppb.ExecutionStrategy_Value_value[strings.ToUpper(cmd.REProxyConfig.ExecStrategy)]; ok {
		strategy = ppb.ExecutionStrategy_Value(res)
	} else {
		return nil, fmt.Errorf("invalid execution strategy %s", cmd.REProxyConfig.ExecStrategy)
	}

	md := &ppb.Metadata{EventTimes: map[string]*cpb.TimeInterval{
		wrapperOverheadKey: {From: command.TimeToProto(time.Now())},
	}}
	md.Environment = os.Environ()

	return &ppb.RunRequest{
		Command: c,
		Labels:  cmd.REProxyConfig.Labels,
		ExecutionOptions: &ppb.ProxyExecutionOptions{
			ExecutionStrategy: strategy,
			CompareWithLocal:  false,
			NumLocalReruns:    0,
			NumRemoteReruns:   0,
			ReclientTimeout:   int32(time.Hour.Seconds()),
			RemoteExecutionOptions: &ppb.RemoteExecutionOptions{
				AcceptCached:                 !cmd.SkipCacheLookup,
				DoNotCache:                   cmd.DoNotCache,
				DownloadOutputs:              cmd.REProxyConfig.DownloadOutputs,
				Wrapper:                      cmd.REProxyConfig.RemoteWrapper,
				CanonicalizeWorkingDir:       cmd.REProxyConfig.CanonicalizeWorkingDir,
				PreserveUnchangedOutputMtime: false,
			},
			LocalExecutionOptions: &ppb.LocalExecutionOptions{
				AcceptCached: true,
				DoNotCache:   false,
			},
			LogEnvironment: false,
			// Necessary for metadata such as digests to be returned.
			IncludeActionLog: true,
		},
		// b/297458470: Do not apply cmd.ToolInputs because Reproxy adds
		// the directories of ToolchainInputs to PATH and it may cause
		// "docker: argument list too long" error.
		ToolchainInputs: nil,
		Metadata:        md,
	}, nil
}

func processResponse(ctx context.Context, cmd *execute.Cmd, response *ppb.RunResponse, err error) error {
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

	// any stdout/stderr is unexpected, write this out and stop if received.
	if len(response.Stdout) > 0 {
		cmd.StdoutWriter().Write(response.Stdout)
	}
	if len(response.Stderr) > 0 {
		cmd.StderrWriter().Write(response.Stderr)
	}
	if d := response.GetActionLog().GetRemoteMetadata().GetActionDigest(); d != "" {
		dg, err := digest.Parse(d)
		if err != nil {
			return err
		}
		cmd.SetActionDigest(dg)
	}
	cmd.SetActionResult(result, cached)
	err = resultErr(response)
	if err != nil {
		return err
	}

	// for now, update hashfs as if this was local exec.
	// https://source.chromium.org/chromium/infra/infra/+/main:go/src/infra/build/siso/execute/localexec/localexec.go;l=58-76;drc=f9a8d895ee9b5d51a5d5da0131de75c021b4a212
	// TODO(b/273407069): use the hashes returned by reproxy.
	if cmd.HashFS == nil {
		return nil
	}
	now := time.Now()
	return cmd.HashFS.UpdateFromLocal(ctx, cmd.ExecRoot, cmd.AllOutputs(), cmd.Restat, now, cmd.CmdHash)
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
