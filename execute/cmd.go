// Copyright 2023 The Chromium Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package execute runs commands.
package execute

import (
	"bytes"
	"context"
	"io"
	"path/filepath"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"

	"infra/build/siso/hashfs"
	"infra/build/siso/reapi/digest"
	"infra/build/siso/reapi/merkletree"
	"infra/build/siso/toolsupport/shutil"
)

// Executor is an interface to run the cmd.
type Executor interface {
	Run(ctx context.Context, cmd *Cmd) error
}

// Cmd includes all the information required to run a build command.
type Cmd struct {
	// ID is used as a unique identifier for this action in logs and tracing.
	// It does not have to be human-readable, so using a UUID is fine.
	ID string

	// Desc is a short, human-readable identifier that is shown to the user when referencing this action in the UI or a log file.
	// Example: "CXX hello.o"
	Desc string

	// ActionName is the name of the rule that generated this action.
	// Example: "cxx" or "link"
	ActionName string

	// Args holds command line arguments.
	Args []string

	// Env specifies the environment of the process.
	Env []string

	// RSPFile is the filename of the response file for the cmd.
	// If set,  Siso will write the RSPFileContent to the file before executing the action, and delete the file after executing the cmd successfully.
	RSPFile string

	// RSPFileContent is the content of the response file for the cmd.
	// The bindings are already expanded.
	RSPFileContent []byte

	// CmdHash is a hash of the command line, which is used to check for changes in the command line since it was last executed.
	CmdHash []byte

	// ExecRoot is an exec root directory of the cmd.
	ExecRoot string

	// Dir specifies the working directory of the cmd,
	// relative to ExecRoot.
	Dir string

	// Inputs are input files of the cmd, relative to ExecRoot.
	// They may be overridden by deps inputs.
	Inputs []string

	// ToolInputs are tool input files of the cmd, relative to ExecRoot.
	// They are specified by the siso config, not overridden by deps.
	// (or inputs would be deps + tool inputs).
	// These are expected to be toolchain input files, not by specified
	// by build deps, nor in deps log.
	ToolInputs []string

	// Outputs are output files of the cmd, relative to ExecRoot.
	Outputs []string

	// Deps specifies deps type of the cmd, "gcc", "msvc".
	Deps string

	// Depfile specifies a filename for dep info, relative to ExecRoot.
	Depfile string

	// DepsArgs are args to get deps.
	// If empty, it will be generated from Args + Deps.
	DepsArgs []string

	// If Restat is true
	// - output files may be used only for inputs
	// - no need to update mtime if content is not changed.
	Restat bool

	// TODO(jwata): implement digest calculation.

	// TODO(jwata): support file trace with strace.

	stdoutWriter, stderrWriter io.Writer
	stdoutBuffer, stderrBuffer bytes.Buffer

	actionResult *rpb.ActionResult
}

// String returns an ID of the cmd.
func (c *Cmd) String() string {
	return c.ID
}

// Command returns a command line string.
func (c *Cmd) Command() string {
	if len(c.Args) == 3 && c.Args[0] == "/bin/sh" && c.Args[1] == "-c" {
		return c.Args[2]
	}
	return shutil.Join(c.Args)
}

// AllInputs returns all inputs of the cmd.
func (c *Cmd) AllInputs() []string {
	if c.RSPFile == "" {
		return c.Inputs
	}
	inputs := make([]string, len(c.Inputs)+1)
	copy(inputs, c.Inputs)
	inputs[len(inputs)-1] = c.RSPFile
	return inputs
}

// AllOutputs returns all outputs of the cmd.
func (c *Cmd) AllOutputs() []string {
	if c.Depfile == "" {
		return c.Outputs
	}
	outputs := make([]string, len(c.Outputs)+1)
	copy(outputs, c.Outputs)
	outputs[len(outputs)-1] = c.Depfile
	return outputs
}

// SetStdoutWriter sets w for stdout.
func (c *Cmd) SetStdoutWriter(w io.Writer) {
	c.stdoutWriter = w
}

// SetStderrWriter sets w for stderr.
func (c *Cmd) SetStderrWriter(w io.Writer) {
	c.stderrWriter = w
}

// StdoutWriter returns a writer set for stdout.
func (c *Cmd) StdoutWriter() io.Writer {
	c.stdoutBuffer.Reset()
	if c.stdoutWriter == nil {
		return &c.stdoutBuffer
	}
	return io.MultiWriter(c.stdoutWriter, &c.stdoutBuffer)
}

// StderrWriter returns a writer set for stderr.
func (c *Cmd) StderrWriter() io.Writer {
	c.stderrBuffer.Reset()
	if c.stderrWriter == nil {
		return &c.stderrBuffer
	}
	return io.MultiWriter(c.stderrWriter, &c.stderrBuffer)
}

// Stdout returns stdout output of the cmd.
func (c *Cmd) Stdout() []byte {
	return c.stdoutBuffer.Bytes()
}

// Stderr returns stderr output of the cmd.
// Since RBE merges stderr into stdout, we won't get stderr for remote actions. b/149501385
// Therefore, we need to be careful how we use stdout/stderr for now.
// For example, if we use /showIncludes to stderr, it will be on stdout from a remote action.
func (c *Cmd) Stderr() []byte {
	return c.stderrBuffer.Bytes()
}

// Digest computes action digest of the cmd.
// If ds is nil, then it will reuse the previous calculated digest if any.
func (c *Cmd) Digest(ctx context.Context, ds *digest.Store) (digest.Digest, error) {
	// TODO(jwata): implement this method.
	return digest.Digest{}, nil
}

// SetActionResults sets action result to the cmd.
func (c *Cmd) SetActionResult(result *rpb.ActionResult) {
	c.actionResult = result
}

// ActionResult returns the action result of the cmd.
func (c *Cmd) ActionResult() *rpb.ActionResult {
	return c.actionResult
}

// EntriesFromResult returns output file entries for the cmd and result.
func (c *Cmd) EntriesFromResult(ctx context.Context, ds hashfs.DataSource, result *rpb.ActionResult) []merkletree.Entry {
	var entries []merkletree.Entry
	for _, f := range result.GetOutputFiles() {
		if f.Digest == nil {
			continue
		}
		fname := filepath.Join(c.Dir, f.Path)
		entries = append(entries, merkletree.Entry{
			Name:         fname,
			Data:         ds.DigestData(digest.FromProto(f.Digest), fname),
			IsExecutable: f.IsExecutable,
		})
	}
	for _, s := range result.GetOutputSymlinks() {
		if s.Target == "" {
			continue
		}
		fname := filepath.Join(c.Dir, s.Path)
		entries = append(entries, merkletree.Entry{
			Name:   fname,
			Target: s.Target,
		})
	}
	for _, d := range result.GetOutputDirectories() {
		// It just needs to add the directories here because it assumes that they have already been expanded by ninja State.
		dname := filepath.Join(c.Dir, d.Path)
		entries = append(entries, merkletree.Entry{
			Name: dname,
		})
	}
	return entries
}
