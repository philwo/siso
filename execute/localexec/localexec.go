// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package localexec implements local command execution.
package localexec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	log "github.com/golang/glog"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"infra/build/siso/execute"
	epb "infra/build/siso/execute/proto"
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
	cmd.SetActionResult(res, false)

	clog.Infof(ctx, "exit=%d stdout=%d stderr=%d metadata=%s", res.ExitCode, len(res.StdoutRaw), len(res.StderrRaw), res.ExecutionMetadata)

	if res.ExitCode != 0 {
		return &execute.ExitError{ExitCode: int(res.ExitCode)}
	}
	if cmd.HashFS == nil {
		return nil
	}
	// TODO(b/254158307): calculate action digest if cmd is pure?
	now := time.Now()
	return cmd.RecordOutputsFromLocal(ctx, now)
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
	c.Stdout = cmd.StdoutWriter()
	c.Stderr = cmd.StderrWriter()
	if cmd.Console {
		c.Stdin = os.Stdin

		// newline after siso status line if cmd outputs.
		checkClose := func(f *os.File, s string) {
			err := f.Close()
			if err != nil {
				clog.Warningf(ctx, "close %s: %v", s, err)
			}
		}
		var wg sync.WaitGroup
		defer wg.Wait()

		stdoutr, stdoutw, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		defer checkClose(stdoutr, "stdout(r)")
		defer checkClose(stdoutw, "stdout(w)")
		stderrr, stderrw, err := os.Pipe()
		if err != nil {
			return nil, err
		}

		defer checkClose(stderrr, "stderr(r)")
		defer checkClose(stderrw, "stderr(w)")
		rctx, cancel := context.WithCancel(ctx)
		defer cancel()
		var outStarted atomic.Bool
		consoleOut := func(r io.Reader, w io.Writer, s string) {
			defer wg.Done()
			var buf [1]byte
			n, err := r.Read(buf[:])
			if err != nil {
				return
			}
			if !outStarted.Swap(true) {
				fmt.Fprintln(w)
			}
			_, err = w.Write(buf[:n])
			if err != nil {
				clog.Warningf(rctx, "write %s: %v", s, err)
			}
			_, err = io.Copy(w, r)
			if err != nil && !errors.Is(err, os.ErrClosed) {
				clog.Warningf(rctx, "copy %s: %v", s, err)
			}
		}

		wg.Add(2)
		go consoleOut(stdoutr, os.Stdout, "stdout")
		go consoleOut(stderrr, os.Stderr, "stderr")

		c.Stdout = io.MultiWriter(stdoutw, c.Stdout)
		c.Stderr = io.MultiWriter(stderrw, c.Stderr)
	}
	s := time.Now()

	var ru *epb.Rusage
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
			ru = rusage(c)

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
		if err == nil {
			ru = rusage(c)
		}
		log.V(1).Infof("%s filetrace=false %v", cmd.ID, err)
	}
	e := time.Now()

	code := exitCode(err)

	result := &rpb.ActionResult{
		ExitCode:  code,
		StdoutRaw: cmd.Stdout(),
		StderrRaw: cmd.Stderr(),
		ExecutionMetadata: &rpb.ExecutedActionMetadata{
			Worker:                      WorkerName,
			ExecutionStartTimestamp:     timestamppb.New(s),
			ExecutionCompletedTimestamp: timestamppb.New(e),
		},
	}
	if ru != nil {
		p, err := anypb.New(ru)
		if err != nil {
			clog.Warningf(ctx, "pack rusage: %v", err)
		} else {
			result.ExecutionMetadata.AuxiliaryMetadata = append(result.ExecutionMetadata.AuxiliaryMetadata, p)
		}
	}

	// TODO(b/273423470): track resource usage.

	if code >= 0 {
		err = nil
	}
	return result, err
}

func exitCode(err error) int32 {
	if err == nil {
		return 0
	}
	var eerr *exec.ExitError
	if !errors.As(err, &eerr) {
		return -1
	}
	return int32(eerr.ProcessState.ExitCode())
}

// TraceEnabled returns whether file trace is enabled or not.
func TraceEnabled(ctx context.Context) bool {
	return straceutil.Available(ctx)
}
