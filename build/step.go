// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"sync"
	"time"

	"infra/build/siso/execute"
)

// Step is a build step.
type Step struct {
	cmd *execute.Cmd

	state *stepState
}

type stepState struct {
	mu               sync.Mutex
	phase            stepPhase
	weightedDuration time.Duration
}

type stepPhase int

func (s *Step) getWeightedDuration() time.Duration {
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	return s.state.weightedDuration
}
