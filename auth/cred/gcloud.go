// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package cred

import (
	"fmt"
	"os/exec"
	"strings"

	"golang.org/x/oauth2"
)

type gcloudTokenSource struct{}

func (gcloudTokenSource) Token() (*oauth2.Token, error) {
	cmd := exec.Command("gcloud", "auth", "print-access-token")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to get token %s: %w", string(out), err)
	}
	token := strings.TrimSpace(string(out))

	return fromTokenString("gcloud", token)
}
