// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package remoteexec executes cmd with remote exec API.
package remoteexec

import (
	"context"
	"errors"
	"fmt"
	"time"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/charmbracelet/log"

	"go.chromium.org/infra/build/siso/execute"
	"go.chromium.org/infra/build/siso/reapi"
	"go.chromium.org/infra/build/siso/reapi/digest"
	"go.chromium.org/infra/build/siso/reapi/merkletree"
)

// RemoteExec is executor with remote exec API.
type RemoteExec struct {
	client *reapi.Client
}

// New creates new remote executor.
func New(client *reapi.Client) *RemoteExec {
	return &RemoteExec{
		client: client,
	}
}

func (re *RemoteExec) prepareInputs(ctx context.Context, cmd *execute.Cmd) (digest.Digest, error) {
	ds := digest.NewStore()
	actionDigest, err := cmd.Digest(ctx, ds)
	if err != nil {
		return actionDigest, err
	}
	_, err = re.client.UploadAll(ctx, ds)
	if err != nil {
		return actionDigest, fmt.Errorf("failed to upload all %s: %w", cmd, err)
	}
	return actionDigest, nil
}

// Run runs a cmd.
func (re *RemoteExec) Run(ctx context.Context, cmd *execute.Cmd) error {
	actionDigest, err := re.prepareInputs(ctx, cmd)
	if err != nil {
		return err
	}

	if cmd.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeoutCause(ctx, cmd.Timeout, fmt.Errorf("remote exec timeout=%v: %w", cmd.Timeout, context.DeadlineExceeded))
		defer cancel()
	}
	_, resp, err := re.client.ExecuteAndWait(ctx, &rpb.ExecuteRequest{
		ActionDigest:    actionDigest.Proto(),
		SkipCacheLookup: cmd.SkipCacheLookup,
	})
	if err != nil {
		log.Warnf("digest: %s, err: %v", actionDigest, err)
	}
	result := resp.GetResult()
	return re.processResult(ctx, cmd, result, resp.GetCachedResult(), err)
}

func (re *RemoteExec) processResult(ctx context.Context, cmd *execute.Cmd, result *rpb.ActionResult, cached bool, err error) error {
	// if result.GetExitCode() == 0 && err == nil {
	// 	log.Infof("exit=%d cache=%t", result.GetExitCode(), cached)
	// } else {
	// 	log.Warnf("exit=%d cache=%t result=%v err:%v", result.GetExitCode(), cached, result, err)
	// }
	if result == nil {
		if err != nil {
			return err
		}
		return errors.New("no result")
	}
	now := time.Now()
	files := result.GetOutputFiles()
	symlinks := result.GetOutputSymlinks()
	var dirs []*rpb.OutputDirectory
	for _, d := range result.GetOutputDirectories() {
		ds := digest.NewStore()
		outdir, derr := re.client.FetchTree(ctx, d.GetPath(), digest.FromProto(d.GetTreeDigest()), ds)
		if derr != nil {
			log.Errorf("Failed to fetch tree %s %s: %v", d.GetPath(), d.GetTreeDigest(), derr)
			continue
		}
		dfiles, dsymlinks, ddirs := merkletree.Traverse(ctx, d.GetPath(), outdir, ds)
		files = append(files, dfiles...)
		symlinks = append(symlinks, dsymlinks...)
		dirs = append(dirs, ddirs...)
	}
	result.OutputFiles = files
	result.OutputSymlinks = symlinks
	result.OutputDirectories = dirs
	cmd.SetActionResult(result, cached)
	if len(result.GetStdoutRaw()) > 0 {
		cmd.StdoutWriter().Write(result.GetStdoutRaw())
	} else {
		stdoutErr := setStdout(ctx, re.client, result.GetStdoutDigest(), cmd)
		if err == nil {
			err = stdoutErr
		}
	}
	if len(result.GetStderrRaw()) > 0 {
		cmd.StderrWriter().Write(result.GetStderrRaw())
	} else {
		stderrErr := setStderr(ctx, re.client, result.GetStderrDigest(), cmd)
		if err == nil {
			err = stderrErr
		}
	}
	if err != nil {
		// Even when err was not nil, the outputs/stdout/stderr were populated to ActionResult for investigation.
		return err
	}
	cmdErr := resultErr(result)
	if cmdErr != nil {
		return cmdErr
	}
	// update output file only step succeeded.
	return cmd.RecordOutputs(ctx, cmd.HashFS.DataSource(), now)
}

// share it with localexec?

func resultErr(result *rpb.ActionResult) error {
	if result == nil {
		return errors.New("no result")
	}
	if result.GetExitCode() == 0 {
		return nil
	}
	return execute.ExitError{
		ExitCode: int(result.GetExitCode()),
	}
}

func setStdout(ctx context.Context, client *reapi.Client, d *rpb.Digest, cmd *execute.Cmd) error {
	if d == nil || d.SizeBytes == 0 {
		return nil
	}
	w := cmd.StdoutWriter()
	b, err := client.Get(ctx, digest.FromProto(d), "stdout")
	if err != nil {
		return err
	}
	w.Write(b)
	return nil
}

func setStderr(ctx context.Context, client *reapi.Client, d *rpb.Digest, cmd *execute.Cmd) error {
	if d == nil || d.SizeBytes == 0 {
		return nil
	}
	w := cmd.StderrWriter()
	b, err := client.Get(ctx, digest.FromProto(d), "stderr")
	if err != nil {
		return err
	}
	w.Write(b)
	return nil
}
