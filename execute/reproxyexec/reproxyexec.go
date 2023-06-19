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
type REProxyExec struct{}

// New creates new remote executor.
func New(ctx context.Context) *REProxyExec {
	return &REProxyExec{}
}

// Run runs a cmd.
func (re *REProxyExec) Run(ctx context.Context, cmd *execute.Cmd) error {
	ctx, span := trace.NewSpan(ctx, "reproxy-exec")
	defer span.Close(nil)

	if len(cmd.REProxyConfig.Labels) == 0 {
		return fmt.Errorf("REProxy config has no labels")
	}
	if len(cmd.REProxyConfig.Platform) == 0 {
		return fmt.Errorf("REProxy config has no platform")
	}
	if cmd.REProxyConfig.ServerAddress == "" {
		return fmt.Errorf("REProxy config has no server address")
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

	// Create gRPC connection with the provided address.
	// TODO(b/273407069): this will be problematic on windows, reuse connections instead.
	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	conn, err := DialContext(dialCtx, cmd.REProxyConfig.ServerAddress)
	if err != nil {
		log.Fatalf("Fail to dial %s: %v", cmd.REProxyConfig.ServerAddress, err)
	}
	defer conn.Close()

	// Create REProxy client and send the request with backoff configuration above.
	// (No timeout applied due to use of backoff with maximum attempts allowed.)
	proxy := ppb.NewCommandsClient(conn)
	req, err := createRequest(ctx, cmd, execTimeout)
	if err != nil {
		return err
	}
	var resp *ppb.RunResponse
	err = retry.WithPolicy(ctx, shouldRetry, backoff, func() error {
		resp, err = proxy.RunCommand(ctx, req)
		return err
	})

	err = re.processResponse(ctx, cmd, resp, err)
	if err != nil {
		log.Errorf("Command failed for cmd %q: %v", cmd.Desc, err)
	}
	return err
}

// Proxy is the interface of the RE Proxy API.
type Proxy interface {
	RunCommand(context.Context, *ppb.RunRequest, ...grpc.CallOption) (*ppb.RunResponse, error)
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

	// Manually override remote_local_fallback to remote.
	// Local fallback should always use our logic, not rewrapper.
	if strategy == ppb.ExecutionStrategy_REMOTE_LOCAL_FALLBACK {
		if log.V(1) {
			clog.Infof(ctx, "overriding reproxy REMOTE_LOCAL_FALLBACK to REMOTE")
		}
		strategy = ppb.ExecutionStrategy_REMOTE
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
				Wrapper:                      cmd.RemoteWrapper,
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
		ToolchainInputs: cmd.ToolInputs,
		Metadata:        md,
	}, nil
}

func (re *REProxyExec) processResponse(ctx context.Context, cmd *execute.Cmd, response *ppb.RunResponse, err error) error {
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
	return cmd.HashFS.UpdateFromLocal(ctx, cmd.ExecRoot, cmd.AllOutputs(), time.Now(), cmd.CmdHash)
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
