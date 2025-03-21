// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package cred

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/golang/glog"
	"golang.org/x/oauth2"
)

var errNoAuthorization = errors.New("no authrozation header")

// credHelper handles bazel credential helper.
// https://github.com/EngFlow/credential-helper-spec/blob/main/spec.md
type credHelper struct {
	path string

	mu    sync.Mutex
	cache map[string]*credCacheEntry
}

// https://github.com/EngFlow/credential-helper-spec/blob/7df9bef60ef05636fd93114a17a7b2ea08143af6/schemas/get-credentials-response.schema.json
type credHelperResp struct {
	Headers map[string][]string `json:"headers"`
	Expires string              `json:"expires"`

	stdout []byte
}

func (h *credHelper) run(ctx context.Context, endpoint string) (credHelperResp, error) {
	cmd := exec.CommandContext(ctx, h.path, "get")
	type credHelperReq struct {
		URI string `json:"uri"`
	}
	req := credHelperReq{URI: endpoint}
	var resp credHelperResp
	buf, err := json.Marshal(req)
	if err != nil {
		return resp, err
	}
	cmd.Stdin = bytes.NewReader(buf)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	if err != nil {
		err = credHelperErr(h.path, err)
		if len(stderr.Bytes()) == 0 {
			return resp, fmt.Errorf("failed to run helper: %w", err)
		}
		return resp, fmt.Errorf("failed to run helper: %w\nstderr: %s", err, stderr.String())
	}
	resp.stdout = stdout.Bytes()
	err = json.Unmarshal(stdout.Bytes(), &resp)
	if err != nil {
		return resp, fmt.Errorf("failed to parse resp from helper %s: %w\nstdout: %s", h.path, err, stdout.String())
	}
	return resp, nil
}

type credCacheEntry struct {
	mu   sync.Mutex
	cred credHelperPerRPCCredentials
}

type credHelperPerRPCCredentials struct {
	headers map[string]string
	expires time.Time
	stdout  []byte
}

func (h *credHelper) get(ctx context.Context, endpoint string) (credHelperPerRPCCredentials, error) {
	if strings.HasPrefix(endpoint, "https://") && strings.Contains(endpoint, ".googleapis.com/") {
		endpoint = "https://*.googleapis.com/"
	}
	h.mu.Lock()
	if h.cache == nil {
		h.cache = make(map[string]*credCacheEntry)
	}
	cce, ok := h.cache[endpoint]
	if !ok {
		cce = &credCacheEntry{}
		h.cache[endpoint] = cce
	}
	h.mu.Unlock()

	cce.mu.Lock()
	defer cce.mu.Unlock()
	if cce.cred.expires.IsZero() || cce.cred.expires.Before(time.Now()) {
		ctx, cancel := context.WithTimeout(ctx, 1*time.Minute)
		defer cancel()
		// first call, or expired
		started := time.Now()
		resp, err := h.run(ctx, endpoint)
		if err != nil {
			return cce.cred, fmt.Errorf("credhelper failed: %w", err)
		}
		expires := time.Now().Add(1 * time.Hour)
		if resp.Expires != "" {
			expires, err = time.Parse(time.RFC3339, resp.Expires)
			if err != nil {
				return cce.cred, fmt.Errorf("failed to parse credhelper expires %q: %v", resp.Expires, err)
			}
		}
		cce.cred.headers = make(map[string]string)
		for k, v := range resp.Headers {
			if len(v) == 0 {
				continue
			}
			cce.cred.headers[strings.ToLower(k)] = strings.Join(v, ",")
		}
		cce.cred.expires = expires
		cce.cred.stdout = resp.stdout
		glog.Infof("cred %s %s valid %s", endpoint, time.Since(started), time.Until(expires))
	}
	return cce.cred, nil
}

func (h *credHelper) GetRequestMetadata(ctx context.Context, uri ...string) (map[string]string, error) {
	endpoint := "https://*.googleapis.com/"
	if len(uri) > 0 {
		endpoint = uri[0]
	}
	prc, err := h.get(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	return prc.headers, nil
}

func (*credHelper) RequireTransportSecurity() bool {
	return true
}

func (h *credHelper) token(ctx context.Context, endpoint string) (*oauth2.Token, error) {
	prc, err := h.get(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	auth := prc.headers["authorization"]
	if auth == "" {
		return nil, fmt.Errorf("%w in resp from helper %s\nstdout: %s", errNoAuthorization, h.path, string(prc.stdout))
	}
	token := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	t := &oauth2.Token{
		AccessToken: token,
		Expiry:      prc.expires,
	}
	t = t.WithExtra(map[string]any{
		"x-token-source": h.path,
	})
	return t, nil
}

type credHelperGoogle struct {
	h *credHelper
}

func (h *credHelperGoogle) Token() (*oauth2.Token, error) {
	return h.h.token(context.Background(), "https://*.googleapis.com/")
}
