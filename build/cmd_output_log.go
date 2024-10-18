// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"fmt"
	"strings"

	"infra/build/siso/execute"
	"infra/build/siso/toolsupport/msvcutil"
	"infra/build/siso/ui"
)

type cmdOutputResult int

const (
	cmdOutputResultFAILED cmdOutputResult = iota
	cmdOutputResultSUCCESS
	cmdOutputResultFALLBACK
	cmdOutputResultRETRY
)

func (r cmdOutputResult) String() string {
	switch r {
	case cmdOutputResultFAILED:
		return "FAILED"
	case cmdOutputResultSUCCESS:
		return "SUCCESS"
	case cmdOutputResultFALLBACK:
		return "FALLBACK"
	case cmdOutputResultRETRY:
		return "RETRY"
	}
	return fmt.Sprintf("cmdOutputResult=%d", int(r))
}

type cmdOutputLog struct {
	// result is cmd result (FAILED/SUCCESS/FALLBACK/RETRY)
	result cmdOutputResult
	// phase is when it finished cmd handling.
	phase string

	cmd      *execute.Cmd
	cmdline  string
	sisoRule string
	output   string
	err      error
	stdout   []byte
	stderr   []byte
}

func (c *cmdOutputLog) phaseText() string {
	if c.phase == "" {
		return ""
	}
	return fmt.Sprintf("[%s]", c.phase)
}

func (c *cmdOutputLog) String() string {
	if c == nil {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s%s: %s %q %s\n", c.result, c.phaseText(), c.cmd, c.output, c.cmd.Desc)
	if c.err != nil {
		fmt.Fprintf(&sb, "err: %v\n", c.err)
		if berr, ok := c.err.(interface{ Backtrace() string }); ok {
			fmt.Fprintf(&sb, "stacktrace: %s\n", berr.Backtrace())
		}
	}
	if c.sisoRule != "" {
		fmt.Fprintf(&sb, "siso_rule: %s\n", c.sisoRule)
	}
	digest := c.cmd.ActionDigest()
	if !digest.IsZero() {
		fmt.Fprintf(&sb, "digest: %s\n", digest.String())
	}
	fmt.Fprintf(&sb, "action: %s\n", c.cmd.ActionName)
	fmt.Fprintf(&sb, "%s\n", c.cmdline)
	rsp := c.cmd.RSPFile
	if rsp != "" {
		fmt.Fprintf(&sb, " %s=%q\n", rsp, c.cmd.RSPFileContent)
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
	return sb.String()
}

func (c *cmdOutputLog) Msg(width int, console, verboseFailure bool) string {
	if c == nil {
		return ""
	}
	var sb strings.Builder
	result := fmt.Sprintf("%s%s: %s %q %s\n", c.result, c.phaseText(), c.cmd, c.output, c.cmd.Desc)
	switch c.result {
	case cmdOutputResultFAILED:
		fmt.Fprint(&sb, ui.SGR(ui.Red, result))
	case cmdOutputResultSUCCESS:
		fmt.Fprint(&sb, ui.SGR(ui.Green, result))
	default:
		fmt.Fprint(&sb, result)
	}
	if c.err != nil {
		fmt.Fprintf(&sb, "err: %v\n", c.err)
	}
	const cmdlineTooLong = "  ...(too long)"
	if verboseFailure || width < len(cmdlineTooLong)-1 {
		fmt.Fprintf(&sb, "%s\n", c.cmdline)
	} else {
		cmdline := c.cmdline
		var cut bool
		if len(cmdline) >= width-len(cmdlineTooLong)-1 {
			cmdline = cmdline[:width-len(cmdlineTooLong)-1]
			cut = true
		}
		fmt.Fprintf(&sb, "%s", cmdline)
		if cut {
			fmt.Fprintf(&sb, "%s\nUse '--verbose_failures' to see the command lines\n", cmdlineTooLong)
		} else {
			fmt.Fprintf(&sb, "\n")
		}
	}
	fmt.Fprintf(&sb, "build step: %s %q\n", c.cmd.ActionName, c.output)
	if c.sisoRule != "" {
		fmt.Fprintf(&sb, "siso_rule: %s\n", c.sisoRule)
	}
	if !console {
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
	return sb.String()
}

// cmdOutput returns cmd output log (result, id, desc, err, action, output, args, stdout, stderr).
// it will return nil if ctx is canceled or success with no stdout/stderr.
func cmdOutput(ctx context.Context, result cmdOutputResult, phase string, cmd *execute.Cmd, cmdline, rule string, err error) *cmdOutputLog {
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

func (b *Builder) logOutput(ctx context.Context, cmdOutput *cmdOutputLog, console bool) {
	if cmdOutput == nil {
		return
	}
	var width int
	tui, ok := ui.Default.(*ui.TermUI)
	if ok {
		width = tui.Width()
	}
	if b.outputLogWriter != nil {
		fmt.Fprint(b.outputLogWriter, cmdOutput.String()+"\f\n")
		if cmdOutput.result == cmdOutputResultFALLBACK {
			return
		}
		ui.Default.PrintLines(cmdOutput.Msg(width, console, b.verboseFailures) + "\n")
		return
	}
	if cmdOutput.result == cmdOutputResultFALLBACK {
		return
	}
	ui.Default.PrintLines("\n", "\n", cmdOutput.Msg(width, console, true)+"\n")
}
