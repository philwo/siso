// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package cred

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

func fromTokenString(token string) (*oauth2.Token, error) {
	resp, err := http.Post("https://oauth2.googleapis.com/tokeninfo", "application/x-www-form-urlencoded", strings.NewReader("access_token="+token))
	if err != nil {
		return nil, fmt.Errorf("failed to get tokeninfo: %w", err)
	}
	defer resp.Body.Close()
	buf, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to get tokeninfo body: %w", err)
	}
	type tokJSON struct {
		Exp   string `json:"exp"`
		Email string `json:"email"`
	}
	var tok tokJSON
	err = json.Unmarshal(buf, &tok)
	if err != nil {
		return nil, fmt.Errorf("failed to parse tokeninfo %q: %w", string(buf), err)
	}
	exp, err := strconv.ParseInt(tok.Exp, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("failed to parse exp %q in %q (token:%q): %w", tok.Exp, string(buf), token, err)
	}
	return &oauth2.Token{
		AccessToken: token,
		Expiry:      time.Unix(exp, 0),
	}, nil
}
