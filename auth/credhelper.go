// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package auth

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/log"
	"golang.org/x/oauth2"
)

// credHelper implements the oauth2.TokenSource interface using the luci-auth credential helper.
type credHelper struct {
	path string
	args []string
}

// NewTokenSource creates a new TokenSource using the luci-auth credential helper.
func NewTokenSource() oauth2.TokenSource {
	path, err := exec.LookPath("luci-auth")
	if err != nil {
		log.Warnf("luci-auth not found in PATH: %v", err)
		return nil
	}

	return credHelper{
		path: path,
		args: []string{
			"token",
			"-scopes-context",
			"-json-output=-",
			"-json-format=bazel",
			"-lifetime=5m",
		},
	}
}

// Token implements oauth2.TokenSource interface.
// It runs the credential helper and parses the output to get the token.
func (h credHelper) Token() (*oauth2.Token, error) {
	cmd := exec.Command(h.path, h.args...)
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

	// An earlier draft of the Bazel credential helper specification, still used by Blaze inside Google.
	type response struct {
		Headers map[string]string `json:"headers"`
		Expires string            `json:"expires"`
	}
	resp := response{}
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		return nil, fmt.Errorf("failed to parse resp from helper %s: %w\nstdout: %s", h.path, err, stdout.String())
	}

	auth, ok := resp.Headers["Authorization"]
	if !ok {
		return nil, fmt.Errorf("no Authorization in resp from helper %s\nstdout: %s", h.path, stdout.String())
	}
	token := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	if resp.Expires != "" {
		exp, err := time.Parse(time.RFC3339, resp.Expires)
		if err == nil {
			t := &oauth2.Token{
				AccessToken: token,
				Expiry:      exp,
			}
			t = t.WithExtra(map[string]any{
				"x-token-source": h.path,
			})
			return t, nil
		}
	}

	r, err := http.Post("https://oauth2.googleapis.com/tokeninfo", "application/x-www-form-urlencoded", strings.NewReader("access_token="+token))
	if err != nil {
		return nil, fmt.Errorf("failed to get tokeninfo: %w", err)
	}
	defer r.Body.Close()
	buf, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to get tokeninfo body: %w", err)
	}

	type tokJSON struct {
		Exp   string `json:"exp"`
		Email string `json:"email"`

		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	var tok tokJSON
	err = json.Unmarshal(buf, &tok)
	if err != nil {
		return nil, fmt.Errorf("failed to parse tokeninfo %q: %w", string(buf), err)
	}
	if tok.Error != "" {
		return nil, fmt.Errorf("token error: %s %s", tok.Error, tok.ErrorDescription)
	}

	exp, err := strconv.ParseInt(tok.Exp, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("failed to parse exp %q in %q (token:%q): %w", tok.Exp, string(buf), token, err)
	}

	t := &oauth2.Token{
		AccessToken: token,
		Expiry:      time.Unix(exp, 0),
	}
	t = t.WithExtra(map[string]any{
		"x-token-source": h.path,
		"x-token-email":  tok.Email,
	})

	return t, nil
}
