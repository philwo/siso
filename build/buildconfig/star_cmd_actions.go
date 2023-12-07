// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package buildconfig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	log "github.com/golang/glog"
	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"

	"infra/build/siso/execute"
)

// starCmdActions returns actions, which contains
//
//	fix(inputs, tool_inputs, outputs): fix the cmd.
//	write(fname, content, is_executable): write a file.
//	copy(src, dst, recursive): copy a file or a dir.
//	symlink(target, linkpath): create a symlink.
//	exit(exit_status, stdout, stderr): finish the cmd.
func starCmdActions(ctx context.Context, cmd *execute.Cmd) starlark.Value {
	receiver := starCmdValue{
		ctx: ctx,
		cmd: cmd,
	}

	actionsFix := starlark.NewBuiltin("fix", starActionsFix).BindReceiver(receiver)
	actionsWrite := starlark.NewBuiltin("write", starActionsWrite).BindReceiver(receiver)
	actionsCopy := starlark.NewBuiltin("copy", starActionsCopy).BindReceiver(receiver)
	actionsSymlink := starlark.NewBuiltin("symlink", starActionsSymlink).BindReceiver(receiver)
	actionsExit := starlark.NewBuiltin("exit", starActionsExit).BindReceiver(receiver)
	return starlarkstruct.FromStringDict(starlark.String("actions"), map[string]starlark.Value{
		"fix":     actionsFix,
		"write":   actionsWrite,
		"copy":    actionsCopy,
		"symlink": actionsSymlink,
		"exit":    actionsExit,
	})
}

type starCmdValue struct {
	ctx context.Context
	cmd *execute.Cmd
}

func (c starCmdValue) String() string {
	return fmt.Sprintf("cmd[%s]", c.cmd)
}

func (starCmdValue) Type() string          { return "execute.Cmd" }
func (starCmdValue) Freeze()               {}
func (starCmdValue) Truth() starlark.Bool  { return starlark.True }
func (starCmdValue) Hash() (uint32, error) { return 0, errors.New("execute.Cmd is not hashable") }

// Starlark function `actions.fix(inputs, tool_inputs, outputs, args, reproxy_config)`
// to fix the command's inputs/outputs/args in the context.
func starActionsFix(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	log.V(1).Infof("actions.fix args=%s kwargs=%s", args, kwargs)
	c, ok := fn.Receiver().(starCmdValue)
	if !ok {
		return starlark.None, fmt.Errorf("unexpected receiver: %v", fn.Receiver())
	}
	var inputsValue, toolInputsValue, outputsValue starlark.Value
	var cmdArgsValue starlark.Value
	var reproxyConfigValue starlark.Value
	err := starlark.UnpackArgs("fix", args, kwargs,
		"inputs?", &inputsValue,
		"tool_inputs?", &toolInputsValue,
		"outputs?", &outputsValue,
		"args?", &cmdArgsValue,
		"reproxy_config?", &reproxyConfigValue)
	if err != nil {
		return starlark.None, err
	}
	var inputs, toolInputs, outputs []string
	if inputsValue != nil {
		inputs, err = unpackList(inputsValue)
		if err != nil {
			return starlark.None, err
		}
	}
	if toolInputsValue != nil {
		toolInputs, err = unpackList(toolInputsValue)
		if err != nil {
			return starlark.None, err
		}
	}
	if outputsValue != nil {
		outputs, err = unpackList(outputsValue)
		if err != nil {
			return starlark.None, err
		}
	}
	var cmdArgs []string
	if cmdArgsValue != nil && cmdArgsValue != starlark.None {
		cmdArgs, err = unpackList(cmdArgsValue)
		if err != nil {
			return starlark.None, err
		}
	}
	var reproxyConfigJSON string
	if reproxyConfigValue != nil && reproxyConfigValue != starlark.None {
		reproxyConfigJSON, ok = starlark.AsString(reproxyConfigValue)
		if !ok {
			return starlark.None, fmt.Errorf("reproxy_config is not a string")
		}
	}
	if inputsValue != nil {
		c.cmd.Inputs = uniqueList(inputs)
	}
	if toolInputsValue != nil {
		c.cmd.ToolInputs = uniqueList(toolInputs)
	}
	if outputsValue != nil {
		c.cmd.Outputs = uniqueList(outputs)
	}
	if cmdArgsValue != nil {
		c.cmd.Args = cmdArgs
	}
	if reproxyConfigValue != nil {
		err := json.Unmarshal([]byte(reproxyConfigJSON), &c.cmd.REProxyConfig)
		if err != nil {
			return starlark.None, fmt.Errorf("failed to parse reproxy_config: %w", err)
		}
	}
	return starlark.None, nil
}

// Starlark function `actions.write(fname, content, is_executable)` to write the content a file in hashfs.
func starActionsWrite(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	log.V(1).Infof("actions.write args=%s kwargs=%s", args, kwargs)
	c, ok := fn.Receiver().(starCmdValue)
	if !ok {
		return starlark.None, fmt.Errorf("unexpected receiver: %v", fn.Receiver())
	}
	var fname string
	var content starlark.Bytes
	var isExecutable bool
	err := starlark.UnpackArgs("write", args, kwargs, "fname", &fname, "content?", &content, "is_executable?", isExecutable)
	if err != nil {
		return starlark.None, err
	}
	err = c.cmd.HashFS.WriteFile(c.ctx, c.cmd.ExecRoot, fname, []byte(string(content)), isExecutable, time.Now(), c.cmd.CmdHash)
	return starlark.None, err
}

// Starlark function `actions.copy(src, dst, recursive)` to copy a file, or a dir, in hashfs.
func starActionsCopy(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	log.V(1).Infof("actions.copy args=%s kwargs=%s", args, kwargs)
	c, ok := fn.Receiver().(starCmdValue)
	if !ok {
		return starlark.None, fmt.Errorf("unexpected receiver: %v", fn.Receiver())
	}
	var src, dst string
	var recursive bool
	err := starlark.UnpackArgs("copy", args, kwargs, "src", &src, "dst", &dst, "recursive?", &recursive)
	if err != nil {
		return starlark.None, err
	}
	if recursive {
		var files []string
		files, err = actionsCopyRecursively(c.ctx, c.cmd, src, dst, time.Now(), c.cmd.CmdHash)
		if err == nil && len(files) > 0 {
			err = c.cmd.HashFS.Flush(c.ctx, c.cmd.ExecRoot, files)
		}
	} else {
		err = c.cmd.HashFS.Copy(c.ctx, c.cmd.ExecRoot, src, dst, time.Now(), c.cmd.CmdHash)
	}
	return starlark.None, err
}

// recursively copy from src to dst by setting mtime t with cmdhash,
// and returns a list of files that needs to be written to the disk.
// if src is a directory, it recurrsively calls itself without cmdhash.
// if src is a file, it just copies the file.
func actionsCopyRecursively(ctx context.Context, cmd *execute.Cmd, src, dst string, t time.Time, cmdhash []byte) ([]string, error) {
	fi, err := cmd.HashFS.Stat(ctx, cmd.ExecRoot, src)
	if err != nil {
		return nil, err
	}
	if fi.IsDir() {
		err := cmd.HashFS.Mkdir(ctx, cmd.ExecRoot, dst, cmdhash)
		if err != nil {
			return nil, err
		}
		ents, err := cmd.HashFS.ReadDir(ctx, cmd.ExecRoot, src)
		if err != nil {
			return nil, err
		}
		var files []string
		for _, ent := range ents {
			s := filepath.Join(src, ent.Name())
			d := filepath.Join(dst, ent.Name())
			// don't record cmdhash for recursive copy
			// since these are not appeared in build graph
			// but cleandead will try to delete these dirs/files.
			f, err := actionsCopyRecursively(ctx, cmd, s, d, t, nil)
			if err != nil {
				return files, err
			}
			files = append(files, f...)
		}
		return files, nil
	}
	err = cmd.HashFS.Copy(ctx, cmd.ExecRoot, src, dst, t, cmdhash)
	if err != nil {
		return nil, err
	}
	if cmd.HashFS.NeedFlush(ctx, cmd.ExecRoot, dst) {
		return []string{dst}, nil
	}
	return nil, nil
}

// Starlark function `actions.symlink(target, linkpath)` to create a symlink in hashfs.
func starActionsSymlink(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	log.V(1).Infof("actions.symlink args=%s kwargs=%s", args, kwargs)
	c, ok := fn.Receiver().(starCmdValue)
	if !ok {
		return starlark.None, fmt.Errorf("unexpected receiver: %v", fn.Receiver())
	}
	var target, linkpath string
	err := starlark.UnpackArgs("symlink", args, kwargs, "target", &target, "linkpath", &linkpath)
	if err != nil {
		return starlark.None, err
	}
	err = c.cmd.HashFS.Symlink(c.ctx, c.cmd.ExecRoot, target, linkpath, time.Now(), c.cmd.CmdHash)
	return starlark.None, err
}

// Starlark function `actions.exit(exit_status, stdout, stderr)` to finish the execution of the command.
// can be used when no actual command invocation is needed, such as stamp, copy.
func starActionsExit(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	log.V(1).Infof("actions.exit args=%s kwargs=%s", args, kwargs)
	c, ok := fn.Receiver().(starCmdValue)
	if !ok {
		return nil, fmt.Errorf("unexpected receiver: %v", fn.Receiver())
	}
	var exitStatus int32
	var stdout, stderr starlark.Bytes
	err := starlark.UnpackArgs("exit", args, kwargs, "exit_status", &exitStatus, "stdout?", &stdout, "stderr?", &stderr)
	if err != nil {
		return starlark.None, err
	}
	result := &rpb.ActionResult{
		ExitCode: exitStatus,
	}
	if stdout != "" {
		result.StdoutRaw = []byte(string(stdout))
	}
	if stderr != "" {
		result.StderrRaw = []byte(string(stderr))
	}
	entries, err := c.cmd.HashFS.Entries(c.ctx, c.cmd.ExecRoot, c.cmd.Outputs)
	if err != nil {
		return starlark.None, err
	}
	execute.ResultFromEntries(result, entries)
	c.cmd.SetActionResult(result, false)
	return starlark.None, nil
}
