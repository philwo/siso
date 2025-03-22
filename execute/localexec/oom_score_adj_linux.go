// Copyright 2025 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build linux

package localexec

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"github.com/charmbracelet/log"
)

func oomScoreAdj(ctx context.Context, pid int, score int) {
	err := os.WriteFile(fmt.Sprintf("/proc/%d/oom_score_adj", pid), strconv.AppendInt(nil, int64(score), 10), 0644)
	if err != nil {
		log.Warnf("failed to set %d/oom_score_adj %d: %v", pid, score, err)
	}
}
