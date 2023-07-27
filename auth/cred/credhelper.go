// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package cred

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"golang.org/x/oauth2"
)

// credHelperTokenSource is a token source from bazel credential helper.
// https://github.com/bazelbuild/Proposals/blob/main/designs/2022-06-07-bazel-credential-helpers.md
type credHelperTokenSource struct {
	credHelper string
}

func (h credHelperTokenSource) Token() (*oauth2.Token, error) {
	cmd := exec.Command(h.credHelper, "get")
	cmd.Stdin = strings.NewReader(`{"uri": "https://*.googleapis.com/"}`)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		if len(stderr.Bytes()) == 0 {
			return nil, fmt.Errorf("failed to run helper: %w", err)
		}
		return nil, fmt.Errorf("failed to run helper: %w\nstderr: %s", err, stderr.String())
	}
	type response struct {
		Headers map[string][]string `json:"headers"`
	}
	var resp response
	err = json.Unmarshal(stdout.Bytes(), &resp)
	if err != nil {
		return nil, fmt.Errorf("failed to parse resp from helper %s: %w\nstdout: %s", h.credHelper, err, stdout.String())
	}
	auth := resp.Headers["Authorization"]
	if len(auth) == 0 {
		return nil, fmt.Errorf("no Authorization in resp from helper %s: %w\nstdout: %s", h.credHelper, err, stdout.String())
	}
	token := strings.TrimSpace(strings.TrimPrefix(auth[0], "Bearer "))
	return fromTokenString(h.credHelper, token)
}
