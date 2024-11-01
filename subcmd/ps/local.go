// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"strings"

	"infra/build/siso/build"
	"infra/build/siso/o11y/clog"
)

type localSource struct {
	wd string
}

func newLocalSource(ctx context.Context, dir string) (*localSource, error) {
	err := os.Chdir(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to chdir %s: %w", dir, err)
	}
	wd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("failed to get wd: %w", err)
	}
	return &localSource{wd: wd}, nil
}

func (s *localSource) location() string {
	return s.wd
}

func (s *localSource) text() string { return "" }

func (s *localSource) fetch(ctx context.Context) ([]build.ActiveStepInfo, error) {
	buf, err := os.ReadFile(".siso_port")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("siso is not running in %s?", s.wd)
		}
		return nil, fmt.Errorf("siso is not running in %s? failed to read .siso_port: %w", s.wd, err)
	}
	resp, err := http.Get(fmt.Sprintf("http://%s/api/active_steps", strings.TrimSpace(string(buf))))
	if err != nil {
		return nil, fmt.Errorf("failed to get active_steps via .siso_port: %w", err)
	}
	defer func() {
		err := resp.Body.Close()
		if err != nil {
			clog.Warningf(ctx, "close %v", err)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("/api/active_steps error: %d %s", resp.StatusCode, resp.Status)
	}
	buf, err = io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("/api/active_steps read error: %w", err)
	}
	var activeSteps []build.ActiveStepInfo
	err = json.Unmarshal(buf, &activeSteps)
	if err != nil {
		return nil, fmt.Errorf("/api/active_steps unmarshal error: %w", err)
	}
	return activeSteps, nil
}
