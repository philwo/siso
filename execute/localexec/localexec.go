// Copyright 2023 The Chromium Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package localexec implements local command execution.
package localexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	log "github.com/golang/glog"
	"google.golang.org/protobuf/types/known/timestamppb"

	"infra/build/siso/execute"
	"infra/build/siso/o11y/clog"
	"infra/build/siso/sync/semaphore"
	"infra/build/siso/toolsupport/straceutil"
)

// TODO(b/270886586): Compare local execution with/without local execution server.

// WorkerName is a name used for worker of the cmd in action result.
const WorkerName = "local"

// LocalExec implements execute.Executor interface that runs commands locally.
type LocalExec struct{}

// Run runs cmd with DefaultExec.
func Run(ctx context.Context, cmd *execute.Cmd) error {
	return LocalExec{}.Run(ctx, cmd)
}

// Run runs a cmd.
func (LocalExec) Run(ctx context.Context, cmd *execute.Cmd) (err error) {
	res, err := run(ctx, cmd)
	if err != nil {
		return err
	}
	cmd.StdoutWriter().Write(res.StdoutRaw)
	cmd.StderrWriter().Write(res.StderrRaw)
	cmd.SetActionResult(res, false)

	clog.Infof(ctx, "exit=%d stdout=%d stderr=%d metadata=%s", res.ExitCode, len(res.StdoutRaw), len(res.StderrRaw), res.ExecutionMetadata)

	if res.ExitCode != 0 {
		return &execute.ExitError{ExitCode: int(res.ExitCode)}
	}
	if cmd.HashFS == nil {
		return nil
	}
	// TODO(b/254158307): calculate action digest if cmd is pure?
	return cmd.HashFS.UpdateFromLocal(ctx, cmd.ExecRoot, cmd.AllOutputs(), time.Now(), cmd.CmdHash)
}

// fix for http://b/278658064 windows: fork/exec: Not enough memory resources are available to process this command.
var forkSema = semaphore.New("fork", runtime.NumCPU())

func run(ctx context.Context, cmd *execute.Cmd) (*rpb.ActionResult, error) {
	if len(cmd.Args) == 0 {
		return nil, fmt.Errorf("no arguments in the command. ID: %s", cmd.ID)
	}
	c := exec.CommandContext(ctx, cmd.Args[0], cmd.Args[1:]...)
	c.Env = cmd.Env
	c.Dir = filepath.Join(cmd.ExecRoot, cmd.Dir)
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	s := time.Now()

	var err error
	if cmd.FileTrace != nil {
		if !straceutil.Available(ctx) {
			return nil, fmt.Errorf("strace is not available")
		}
		st := straceutil.New(ctx, cmd.ID, c)
		c = st.Cmd(ctx)
		err = forkSema.Do(ctx, func(ctx context.Context) error {
			return c.Start()
		})
		if err == nil {
			err = c.Wait()
		}
		if err == nil {
			cmd.FileTrace.Inputs, cmd.FileTrace.Outputs, err = st.PostProcess(ctx)
			if err != nil {
				err = fmt.Errorf("failed to postprocess: %w", err)
			}
		}
		st.Close(ctx)
		log.V(1).Infof("%s filetrace=false n_traced_inputs=%d n_traced_outputs=%d err=%v", cmd.ID, len(cmd.Inputs), len(cmd.Outputs), err)
	} else {
		err = forkSema.Do(ctx, func(ctx context.Context) error {
			return c.Start()
		})
		if err == nil {
			err = c.Wait()
		}
		log.V(1).Infof("%s filetrace=false %v", cmd.ID, err)
	}
	e := time.Now()

	result := &rpb.ActionResult{
		ExitCode:  exitCode(err),
		StdoutRaw: stdout.Bytes(),
		StderrRaw: stderr.Bytes(),
		ExecutionMetadata: &rpb.ExecutedActionMetadata{
			Worker:                      WorkerName,
			ExecutionStartTimestamp:     timestamppb.New(s),
			ExecutionCompletedTimestamp: timestamppb.New(e),
		},
	}
	if result.ExitCode != 0 {
		result.StderrRaw = append(result.StderrRaw, []byte(fmt.Sprintf("\ncmd: %q env: %q dir: %q error: %v", cmd.Args, cmd.Env, cmd.Dir, err))...)
	}

	// TODO(b/273423470): track resource usage.

	return result, nil
}

func exitCode(err error) int32 {
	if err == nil {
		return 0
	}
	var eerr *exec.ExitError
	if !errors.As(err, &eerr) {
		return 1
	}
	if w, ok := eerr.ProcessState.Sys().(syscall.WaitStatus); ok {
		return int32(w.ExitStatus())
	}
	return 1
}

// TraceEnabled returns whether file trace is enabled or not.
func TraceEnabled(ctx context.Context) bool {
	return straceutil.Available(ctx)
}
