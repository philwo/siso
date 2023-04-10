// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package remoteexec executes cmd with remote exec API.
package remoteexec

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"time"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	log "github.com/golang/glog"

	"infra/build/siso/execute"
	"infra/build/siso/o11y/clog"
	"infra/build/siso/reapi"
	"infra/build/siso/reapi/digest"
	"infra/build/siso/reapi/merkletree"
	"infra/build/siso/sync/semaphore"
)

// Semaphore enforces a limit on parallel digest calculations to prevent an OOM.
var Semaphore = semaphore.New("remoteexec-digest", runtime.NumCPU()*10)

// RemoteExec is executor with remote exec API.
type RemoteExec struct {
	client *reapi.Client
}

// New creates new remote executor.
func New(ctx context.Context, client *reapi.Client) *RemoteExec {
	return &RemoteExec{
		client: client,
	}
}

func (re *RemoteExec) prepareInputs(ctx context.Context, cmd *execute.Cmd) (digest.Digest, error) {
	var actionDigest digest.Digest
	err := Semaphore.Do(ctx, func(ctx context.Context) error {
		var err error
		ds := digest.NewStore()
		actionDigest, err = cmd.Digest(ctx, ds)
		if err != nil {
			return err
		}
		n, err := re.client.UploadAll(ctx, ds)
		if err != nil {
			return fmt.Errorf("failed to upload all %s: %w", cmd, err)
		}
		clog.Infof(ctx, "upload %d/%d", n, ds.Size())
		return nil
	})
	return actionDigest, err
}

// Run runs a cmd.
func (re *RemoteExec) Run(ctx context.Context, cmd *execute.Cmd) error {
	actionDigest, err := re.prepareInputs(ctx, cmd)
	if err != nil {
		return err
	}

	opName, resp, err := re.client.ExecuteAndWait(ctx, &rpb.ExecuteRequest{
		ActionDigest:    actionDigest.Proto(),
		SkipCacheLookup: cmd.SkipCacheLookup,
	})
	clog.Infof(ctx, "digest: %s, opName: %s", actionDigest, opName)
	if log.V(1) {
		clog.Infof(ctx, "response: %s", resp)
	}
	if err != nil {
		clog.Warningf(ctx, "digest: %s, err: %v", actionDigest, err)
	}
	result := resp.GetResult()

	// TODO(b/267576561): Record execution metadata after integrating with Cloud trace.

	return re.processResult(ctx, actionDigest, cmd, result, resp.GetCachedResult(), err)
}

func (re *RemoteExec) processResult(ctx context.Context, action digest.Digest, cmd *execute.Cmd, result *rpb.ActionResult, cached bool, err error) error {
	if result.GetExitCode() == 0 && err == nil {
		clog.Infof(ctx, "exit=%d cache=%t", result.GetExitCode(), cached)
	} else {
		clog.Warningf(ctx, "exit=%d cache=%t result=%v err:%v", result.GetExitCode(), cached, result, err)
	}
	if result == nil {
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
			clog.Errorf(ctx, "Failed to fetch tree %s %s: %v", d.GetPath(), d.GetTreeDigest(), derr)
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
	if err != nil {
		// Even when err was not nil, the outputs were populated to ActionResult for investigation.
		return err
	}
	entries := cmd.EntriesFromResult(ctx, cmd.HashFS.DataSource(), result)
	clog.Infof(ctx, "output entries %d", len(entries))
	err = cmd.HashFS.Update(ctx, cmd.ExecRoot, entries, now, cmd.CmdHash, action)
	if err != nil {
		return err
	}
	if len(result.GetStdoutRaw()) > 0 {
		cmd.StdoutWriter().Write(result.GetStdoutRaw())
	} else {
		setStdout(ctx, re.client, result.GetStdoutDigest(), cmd)
	}
	if len(result.GetStderrRaw()) > 0 {
		cmd.StderrWriter().Write(result.GetStderrRaw())
	} else {
		setStderr(ctx, re.client, result.GetStderrDigest(), cmd)
	}
	err = resultErr(result)
	if err != nil {
		return err
	}
	cmd.SetActionResult(result)
	return nil
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

func setStdout(ctx context.Context, client *reapi.Client, d *rpb.Digest, cmd *execute.Cmd) {
	if d == nil || d.SizeBytes == 0 {
		return
	}
	w := cmd.StdoutWriter()
	b, err := client.Get(ctx, digest.FromProto(d), "stdout")
	if err != nil {
		// If the context is canceled, we should not write the error to stdout.
		// Otherwise, we will receive a lot of "ctx canceled" error messages to stdout.
		// This is because if one action fails, all concurrent actions will be canceled.
		if ctx.Err() != nil {
			clog.Warningf(ctx, "failed to get stdout: %v", err)
			return
		}
		w.Write([]byte(err.Error()))
	}
	w.Write(b)
}

func setStderr(ctx context.Context, client *reapi.Client, d *rpb.Digest, cmd *execute.Cmd) {
	if d == nil || d.SizeBytes == 0 {
		return
	}
	w := cmd.StderrWriter()
	b, err := client.Get(ctx, digest.FromProto(d), "stderr")
	if err != nil {
		// Do not write the error to stderr for the same reason with stdout.
		if ctx.Err() != nil {
			clog.Warningf(ctx, "failed to get stderr: %v", err)
			return
		}
		w.Write([]byte(err.Error()))
	}
	w.Write(b)
}
