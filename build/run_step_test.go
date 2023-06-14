// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"infra/build/siso/execute"
)

func TestCmdOutput(t *testing.T) {
	execcmd := &execute.Cmd{
		Desc:       "CXX foo.o",
		ActionName: "cxx",
		Args:       []string{"clang++", "-c", "foo.cc", "-o", "foo.o"},
		Dir:        "out/siso",
		Outputs:    []string{"out/siso/foo.o"},
	}
	checkMsg := func(msgs []string, result, desc, actionName, output string, err error) error {
		if len(msgs) < 2 {
			return fmt.Errorf("msgs=%d; want >=2", len(msgs))
		}
		var errs []error
		msg := msgs[0]
		if !strings.Contains(msg, result) {
			errs = append(errs, fmt.Errorf("want result=%q", result))
		}
		if !strings.Contains(msg, desc) {
			errs = append(errs, fmt.Errorf("want desc=%q", desc))
		}
		msg = msgs[1]
		if !strings.Contains(msg, actionName) {
			errs = append(errs, fmt.Errorf("want action=%q", actionName))
		}
		if !strings.Contains(msg, output) {
			errs = append(errs, fmt.Errorf("want output=%q", output))
		}
		if err != nil && !strings.Contains(msg, err.Error()) {
			errs = append(errs, fmt.Errorf("want err=%q", err))
		}
		if len(errs) > 0 {
			return fmt.Errorf("msg=%q; %w", msg, errors.Join(errs...))
		}
		return nil
	}
	for _, tc := range []struct {
		name           string
		result         string
		stdout, stderr []byte
		err            error
		check          func([]string) error
	}{
		{
			name:   "successNoOutErr",
			result: "SUCCESS",
			check: func(msgs []string) error {
				if len(msgs) != 0 {
					return fmt.Errorf("msgs=%q; want empty", msgs)
				}
				return nil
			},
		},
		{
			name:   "successOut",
			result: "SUCCESS",
			stdout: []byte("warning: warning message\n"),
			check: func(msgs []string) error {
				var errs []error
				err := checkMsg(msgs, "SUCCESS", "CXX foo.o", "cxx", "./foo.o", nil)
				if err != nil {
					errs = append(errs, err)
				}
				ok := false
				for _, msg := range msgs {
					if strings.Contains(msg, "stdout:") && strings.Contains(msg, "warning: warning message") {
						ok = true
					}
				}
				if !ok {
					errs = append(errs, fmt.Errorf("missing stdout data:\n%q", msgs))
				}
				if len(errs) > 0 {
					return errors.Join(errs...)
				}
				return nil
			},
		},
		{
			name:   "successErr",
			result: "SUCCESS",
			stderr: []byte("error: error message\n"),
			check: func(msgs []string) error {
				var errs []error
				err := checkMsg(msgs, "SUCCESS", "CXX foo.o", "cxx", "./foo.o", nil)
				if err != nil {
					errs = append(errs, err)
				}
				ok := false
				for _, msg := range msgs {
					if strings.Contains(msg, "stderr:") && strings.Contains(msg, "error: error message") {
						ok = true
					}
				}
				if !ok {
					errs = append(errs, fmt.Errorf("missing stderr data:\n%q", msgs))
				}
				if len(errs) > 0 {
					return errors.Join(errs...)
				}
				return nil
			},
		},
		{
			name:   "error",
			result: "FAILED",
			err:    errors.New("failed to exec: exit=1"),
			check: func(msgs []string) error {
				return checkMsg(msgs, "FAILED", "CXX foo.o", "cxx", "./foo.o", errors.New("failed to exec: exit=1"))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			cmd := &execute.Cmd{}
			*cmd = *execcmd
			if len(tc.stdout) > 0 {
				w := cmd.StdoutWriter()
				w.Write(tc.stdout)
			}
			if len(tc.stderr) > 0 {
				w := cmd.StderrWriter()
				w.Write(tc.stderr)
			}
			msgs := cmdOutput(ctx, tc.result, cmd, tc.err)
			err := tc.check(msgs)
			if err != nil {
				t.Error(err)
			}
		})
	}
}
