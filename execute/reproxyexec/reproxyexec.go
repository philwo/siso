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
	"path/filepath"
	"strings"
	"sync"
	"time"

	lpb "github.com/bazelbuild/reclient/api/log"
	ppb "github.com/bazelbuild/reclient/api/proxy"
	cpb "github.com/bazelbuild/remote-apis-sdks/go/api/command"
	"github.com/bazelbuild/remote-apis-sdks/go/pkg/command"
	"github.com/bazelbuild/remote-apis-sdks/go/pkg/retry"
	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"infra/build/siso/execute"
	"infra/build/siso/o11y/clog"
	"infra/build/siso/o11y/iometrics"
	"infra/build/siso/o11y/trace"
	"infra/build/siso/reapi/digest"
)

const (
	// WorkerNameRemote is a worker name used in ActionResult.ExecutionMetadata for remote execution result.
	WorkerNameRemote = "reproxy-remote"
	// WorkerNameLocal is a worker name used in ActionResult.ExecutionMetadata for local execution result.
	WorkerNameLocal = "reproxy-local"
	// WorkerNameFallback is a worker name used in ActionResult.ExecutionMetadata for local fallback result.
	WorkerNameFallback = "reproxy-fallback"
	// WorkerNameRacingLocal is a worker name used in ActionResult.ExecutionMetadata for racing local result.
	WorkerNameRacingLocal = "reproxy-racing-local"

	// dialTimeout defines the timeout we'd like to use to dial reproxy.
	dialTimeout = 3 * time.Minute
	// defaultExecTimeout defines the default timeout to use for executions.
	defaultExecTimeout = time.Hour
	// defaultReclientTimeout defines the default timeout to use for reproxy request.
	defaultReclientTimeout = time.Hour
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
		case codes.Canceled, codes.Unknown, codes.DeadlineExceeded, codes.Aborted, codes.Unavailable:
			return true
		default:
			// don't retry for codes.ResourceExhausted. i.e. request too large
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

// Used returns whether reproxy is used or not.
func (re *REProxyExec) Used() bool {
	return re.conn != nil
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
			return fmt.Errorf("failed to parse exec_timeout %q: %w", cmd.REProxyConfig.ExecTimeout, err)
		}
		if parsed == 0 {
			return fmt.Errorf("0 is not a valid REProxy exec_timeout")
		}
		execTimeout = parsed
	}
	reclientTimeout := defaultReclientTimeout
	if cmd.REProxyConfig.ReclientTimeout != "" {
		parsed, err := time.ParseDuration(cmd.REProxyConfig.ReclientTimeout)
		if err != nil {
			return fmt.Errorf("failed to parse reclient_timeout %q: %w", cmd.REProxyConfig.ReclientTimeout, err)
		}
		if parsed == 0 {
			return fmt.Errorf("0 is not a valid REProxy reclient_timeout")
		}
		reclientTimeout = parsed
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
	req, err := createRequest(ctx, cmd, execTimeout, reclientTimeout)
	if err != nil {
		return err
	}
	var resp *ppb.RunResponse
	err = retry.WithPolicy(ctx, shouldRetry, backoff, func() error {
		resp, err = proxy.RunCommand(ctx, req)
		return err
	})
	if err != nil {
		return err
	}
	err = processResponse(ctx, cmd, resp)
	if err != nil {
		clog.Warningf(ctx, "Command failed for cmd %q: %v", cmd.Desc, err)
	}
	return err
}

func createRequest(ctx context.Context, cmd *execute.Cmd, execTimeout, reclientTimeout time.Duration) (*ppb.RunRequest, error) {
	args, err := cmd.RemoteArgs()
	if err != nil {
		return nil, err
	}

	var inputs []string
	// don't pass labels or err-file as inputs to reproxy
	// e.g. cmd.REPRoxyConfig.Inputs may contains labels.
	// https://chromium.googlesource.com/chromium/src/+/f640920f763cab187188ab3806fd1a2514068f68/build/config/siso/reproxy.star#245
	checkInputs := func(in string) bool {
		if strings.Contains(in, ":") {
			return false
		}
		_, err := cmd.HashFS.Stat(ctx, cmd.ExecRoot, in)
		return err == nil
	}
	for _, in := range cmd.REProxyConfig.Inputs {
		if checkInputs(in) {
			inputs = append(inputs, in)
		}
	}
	// cmd.AllInputs are already checked
	inputs = append(inputs, cmd.AllInputs()...)

	c := &cpb.Command{
		Identifiers: &cpb.Identifiers{
			CommandId: cmd.ID,
		},
		ExecRoot: cmd.ExecRoot,
		Input: &cpb.InputSpec{
			Inputs: inputs,
		},
		Output: &cpb.OutputSpec{
			OutputFiles: cmd.AllOutputs(),
		},
		Args:             args,
		ExecutionTimeout: int32(execTimeout.Seconds()),
		WorkingDirectory: cmd.Dir,
		Platform:         cmd.REProxyConfig.Platform,
	}
	if cmd.REProxyConfig.PreserveSymlinks {
		c.Input.SymlinkBehavior = cpb.SymlinkBehaviorType_PRESERVE
	}

	// Use exec strategy if found, otherwise fallback to unspecified.
	strategy := ppb.ExecutionStrategy_UNSPECIFIED
	if strategyEnv := os.Getenv("RBE_exec_strategy"); strategyEnv != "" {
		if res, ok := ppb.ExecutionStrategy_Value_value[strings.ToUpper(strategyEnv)]; ok {
			strategy = ppb.ExecutionStrategy_Value(res)
		} else {
			return nil, fmt.Errorf("invalid execution strategy environment variable. RBE_exec_strategy=%s", strategyEnv)
		}
	} else if strategyConf := cmd.REProxyConfig.ExecStrategy; strategyConf != "" {
		if res, ok := ppb.ExecutionStrategy_Value_value[strings.ToUpper(strategyConf)]; ok {
			strategy = ppb.ExecutionStrategy_Value(res)
		} else {
			return nil, fmt.Errorf("invalid execution strategy config. exec_strategy=%s", strategyConf)
		}
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
			ReclientTimeout:   int32(reclientTimeout.Seconds()),
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

func processResponse(ctx context.Context, cmd *execute.Cmd, response *ppb.RunResponse) error {
	if response == nil {
		return errors.New("no response")
	}

	// Log Reproxy's execution ID to associate it with Siso's command ID for debugging purposes.
	clog.Infof(ctx, "RunResponse.ExecutionId=%s", response.GetExecutionId())

	cached := response.GetResult().GetStatus() == cpb.CommandResultStatus_CACHE_HIT
	if response.GetResult().GetExitCode() == 0 {
		clog.Infof(ctx, "exit=%d cache=%t", response.GetResult().GetExitCode(), cached)
	} else {
		clog.Warningf(ctx, "exit=%d cache=%t response=%v", response.GetResult().GetExitCode(), cached, response)
	}

	// TODO(b/273407069): this is nowhere near a complete ActionResult. LogRecord has lots of info, add that info.
	result := &rpb.ActionResult{
		ExitCode:  response.GetResult().GetExitCode(),
		StdoutRaw: response.GetStdout(),
		StderrRaw: response.GetStderr(),
		// TODO(b/273407069): this is nowhere near a complete ExecutedActionMetadata. add extra info where siso needs it.
		ExecutionMetadata: &rpb.ExecutedActionMetadata{},
	}
	al := response.GetActionLog()
	// Completion status
	remoteSuccess := false
	switch cs := al.GetCompletionStatus(); cs {
	case lpb.CompletionStatus_STATUS_CACHE_HIT, lpb.CompletionStatus_STATUS_REMOTE_EXECUTION, lpb.CompletionStatus_STATUS_RACING_REMOTE:
		// remote success
		result.ExecutionMetadata.Worker = WorkerNameRemote
		remoteSuccess = true
	case lpb.CompletionStatus_STATUS_REMOTE_FAILURE, lpb.CompletionStatus_STATUS_NON_ZERO_EXIT, lpb.CompletionStatus_STATUS_TIMEOUT, lpb.CompletionStatus_STATUS_INTERRUPTED:
		// remote failure
		result.ExecutionMetadata.Worker = WorkerNameRemote
	case lpb.CompletionStatus_STATUS_LOCAL_FALLBACK:
		result.ExecutionMetadata.Worker = WorkerNameFallback
	case lpb.CompletionStatus_STATUS_RACING_LOCAL:
		result.ExecutionMetadata.Worker = WorkerNameRacingLocal
	case lpb.CompletionStatus_STATUS_LOCAL_EXECUTION, lpb.CompletionStatus_STATUS_LOCAL_FAILURE:
		result.ExecutionMetadata.Worker = WorkerNameLocal
	}
	// ActionDigest
	if d := al.GetRemoteMetadata().GetActionDigest(); d != "" {
		dg, err := digest.Parse(d)
		if err != nil {
			return err
		}
		cmd.SetActionDigest(dg)
	}
	// Outputs
	updatedTime := time.Now()
	if remoteSuccess {
		err := updateHashFSFromReproxy(ctx, cmd, al, result, updatedTime)
		if err != nil {
			return fmt.Errorf("failed to update hashfs from reproxy outputs: %w", err)
		}
	} else {
		err := cmd.HashFS.UpdateFromLocal(ctx, cmd.ExecRoot, cmd.AllOutputs(), cmd.Restat, updatedTime, cmd.CmdHash)
		if err != nil {
			return fmt.Errorf("failed to update hashfs from local: %w", err)
		}
	}

	if fallbackInfo := response.GetRemoteFallbackInfo(); fallbackInfo != nil {
		cmd.SetRemoteFallbackResult(&rpb.ActionResult{
			ExitCode:  fallbackInfo.GetExitCode(),
			StdoutRaw: fallbackInfo.GetStdout(),
			StderrRaw: fallbackInfo.GetStderr(),
		})
	}

	// any stdout/stderr is unexpected, write this out and stop if received.
	if len(response.Stdout) > 0 {
		cmd.StdoutWriter().Write(response.Stdout)
	}
	if len(response.Stderr) > 0 {
		cmd.StderrWriter().Write(response.Stderr)
	}
	cmd.SetActionResult(result, cached)
	return resultErr(response)
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

// reproxyOutputsDataSource implements fs.DataStore for Reproxy's outputs.
// This allows cmd.EntriesFromResult()/cmd.HashFS.Update() to skip calculating
// digests.
type reproxyOutputsDataSource struct {
	execRoot  string
	iometrics *iometrics.IOMetrics
}

func (ds reproxyOutputsDataSource) Source(_ digest.Digest, fname string) digest.Source {
	path := filepath.Join(ds.execRoot, fname)
	return digest.LocalFileSource{Fname: path, IOMetrics: ds.iometrics}
}

// updateHashFSFromReproxy updates hashfs with the digests provided by Reproxy.
func updateHashFSFromReproxy(ctx context.Context, cmd *execute.Cmd, actionLog *lpb.LogRecord, actionResult *rpb.ActionResult, updatedTime time.Time) error {
	rm := actionLog.GetRemoteMetadata()
	for path, dg := range rm.GetOutputFileDigests() {
		d, err := digest.Parse(dg)
		if err != nil {
			return err
		}
		out := &rpb.OutputFile{
			Path:   path,
			Digest: d.Proto(),
			// TODO: b/303551128 - Fix Reproxy's RunResponse to include IsExecutable.
		}
		actionResult.OutputFiles = append(actionResult.OutputFiles, out)
	}
	for path, dg := range rm.GetOutputDirectoryDigests() {
		d, err := digest.Parse(dg)
		if err != nil {
			return err
		}
		out := &rpb.OutputDirectory{
			Path:       path,
			TreeDigest: d.Proto(),
		}
		actionResult.OutputDirectories = append(actionResult.OutputDirectories, out)
	}
	// TODO: Should handle output symlinks if they are used in Chromium builds?
	ds := &reproxyOutputsDataSource{execRoot: cmd.ExecRoot, iometrics: cmd.HashFS.IOMetrics}
	entries := cmd.EntriesFromResult(ctx, ds, actionResult)
	clog.Infof(ctx, "remote output entries %d", len(entries))
	return cmd.HashFS.Update(ctx, cmd.ExecRoot, entries, updatedTime, cmd.CmdHash, cmd.ActionDigest())
}
