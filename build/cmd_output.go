// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"fmt"
	"strings"

	"go.chromium.org/infra/build/siso/execute"
	"go.chromium.org/infra/build/siso/toolsupport/msvcutil"
	"go.chromium.org/infra/build/siso/ui"
)

type cmdOutputResult int

const (
	cmdOutputResultFAILED cmdOutputResult = iota
	cmdOutputResultSUCCESS
	cmdOutputResultRETRY
)

func (r cmdOutputResult) String() string {
	switch r {
	case cmdOutputResultFAILED:
		return "FAILED"
	case cmdOutputResultSUCCESS:
		return "SUCCESS"
	case cmdOutputResultRETRY:
		return "RETRY"
	}
	return fmt.Sprintf("cmdOutputResult=%d", int(r))
}

type cmdOutputLog struct {
	// result is cmd result (FAILED/SUCCESS/RETRY)
	result cmdOutputResult

	cmd      *execute.Cmd
	cmdline  string
	sisoRule string
	output   string
	err      error
	stdout   []byte
	stderr   []byte
}

func (c *cmdOutputLog) Msg(console bool) string {
	if c == nil {
		return ""
	}
	var sb strings.Builder
	cmdStdoutStderr := func() {
		if console {
			// for console action, stdout/stderr already printed out.
			return
		}
		if len(c.stdout) > 0 {
			fmt.Fprintf(&sb, "stdout:\n%s", string(c.stdout))
			if c.stdout[len(c.stdout)-1] != '\n' {
				fmt.Fprintf(&sb, "\n")
			}
		}
		if len(c.stderr) > 0 {
			fmt.Fprintf(&sb, "stderr:\n%s", string(c.stderr))
			if c.stderr[len(c.stderr)-1] != '\n' {
				fmt.Fprintf(&sb, "\n")
			}
		}
	}

	if c.result == cmdOutputResultSUCCESS {
		// just print stdout/stderr for success result
		cmdStdoutStderr()
		return sb.String()
	}

	result := fmt.Sprintf("%s: %s %q %s\n", c.result, c.cmd, c.output, c.cmd.Desc)
	switch c.result {
	case cmdOutputResultFAILED:
		fmt.Fprint(&sb, ui.SGR(ui.Red, result))
	default:
		fmt.Fprint(&sb, result)
	}
	if c.err != nil {
		fmt.Fprintf(&sb, "err: %v\n", c.err)
	}
	fmt.Fprintf(&sb, "%s\n", c.cmdline)
	fmt.Fprintf(&sb, "build step: %s %q\n", c.cmd.ActionName, c.output)
	if c.sisoRule != "" {
		fmt.Fprintf(&sb, "siso_rule: %s\n", c.sisoRule)
	}
	cmdStdoutStderr()
	return sb.String()
}

// cmdOutput returns cmd output log (result, id, desc, err, action, output, args, stdout, stderr).
// it will return nil if ctx is canceled or success with no stdout/stderr.
func cmdOutput(ctx context.Context, result cmdOutputResult, cmd *execute.Cmd, cmdline, rule string, err error) *cmdOutputLog {
	if ctx.Err() != nil {
		return nil
	}
	stdout := cmd.Stdout()
	stderr := cmd.Stderr()
	if cmd.Deps == "msvc" {
		// cl.exe, clang-cl shows included file to stderr
		// but RBE merges stderr into stdout...
		_, stdout = msvcutil.ParseShowIncludes(stdout)
		_, stderr = msvcutil.ParseShowIncludes(stderr)
	}
	if err == nil && len(stdout) == 0 && len(stderr) == 0 {
		return nil
	}
	res := &cmdOutputLog{
		result:   result,
		cmd:      cmd,
		cmdline:  cmdline,
		sisoRule: rule,
		err:      err,
		stdout:   stdout,
		stderr:   stderr,
	}
	if len(cmd.Outputs) > 0 {
		output := cmd.Outputs[0]
		if strings.HasPrefix(output, cmd.Dir+"/") {
			output = "./" + strings.TrimPrefix(output, cmd.Dir+"/")
		}
		res.output = output
	}
	return res
}

func (b *Builder) logOutput(cmdOutput *cmdOutputLog, console bool) string {
	if cmdOutput == nil {
		return ""
	}
	return cmdOutput.Msg(console)
}
