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
	"time"

	"golang.org/x/oauth2"
)

// credHelperTokenSource is a token source using a credential helper.
type credHelperTokenSource struct {
	credHelper string
	args       []string
}

// The final, approved design for Bazel's credential helper specification:
// https://github.com/EngFlow/credential-helper-spec/blob/main/schemas/get-credentials-response.schema.json
type responseBazelStyle struct {
	Hdrs   map[string][]string `json:"headers"`
	Expiry string              `json:"expires"`
}

func (r responseBazelStyle) Headers() map[string][]string {
	return r.Hdrs
}
func (r responseBazelStyle) Expires() string {
	return r.Expiry
}

// An earlier draft of the Bazel credential helper specification, still used by Blaze inside Google.
// Similar to responseBazelStyle, but uses simple string values instead of []string.
type responseBlazeStyle struct {
	Hdrs   map[string]string `json:"headers"`
	Expiry string            `json:"expires"`
}

func (r responseBlazeStyle) Headers() map[string][]string {
	hdrs := make(map[string][]string)
	for k, v := range r.Hdrs {
		hdrs[k] = []string{v}
	}
	return hdrs
}
func (r responseBlazeStyle) Expires() string {
	return r.Expiry
}

type response interface {
	Headers() map[string][]string
	Expires() string
}

func tryParse(js []byte) (response, error) {
	bazelResp := responseBazelStyle{}
	if err := json.Unmarshal(js, &bazelResp); err == nil {
		return bazelResp, nil
	}
	blazeResp := responseBlazeStyle{}
	if err := json.Unmarshal(js, &blazeResp); err == nil {
		return blazeResp, nil
	}
	return nil, fmt.Errorf("unknown format")
}

func (h credHelperTokenSource) Token() (*oauth2.Token, error) {
	cmd := exec.Command(h.credHelper, h.args...)
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
	resp, err := tryParse(stdout.Bytes())
	if err != nil {
		return nil, fmt.Errorf("failed to parse resp from helper %s: %w\nstdout: %s", h.credHelper, err, stdout.String())
	}

	auth := resp.Headers()["Authorization"]
	if len(auth) == 0 {
		return nil, fmt.Errorf("no Authorization in resp from helper %s\nstdout: %s", h.credHelper, stdout.String())
	}
	token := strings.TrimSpace(strings.TrimPrefix(auth[0], "Bearer "))
	if resp.Expires() != "" {
		exp, err := time.Parse(time.RFC3339, resp.Expires())
		if err == nil {
			t := &oauth2.Token{
				AccessToken: token,
				Expiry:      exp,
			}
			t = t.WithExtra(map[string]any{
				"x-token-source": h.credHelper,
			})
			return t, nil
		}
	}
	return fromTokenString(h.credHelper, token)
}
