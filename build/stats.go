// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"sync"
)

type stats struct {
	mu sync.Mutex
	s  Stats
}

func newStats(total int) *stats {
	return &stats{
		s: Stats{
			Total: total,
		},
	}
}

func (s *stats) update(ctx context.Context, m *StepMetric, pure bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.s.Done++
	if m.skip {
		s.s.Skipped++
	}
	if m.DepsLog {
		if !m.DepsLogErr {
			s.s.FastDepsSuccess++
		} else {
			s.s.FastDepsFailed++
		}
	}
	if m.ScandepsErr {
		s.s.ScanDepsFailed++
	}
	if m.Cached {
		s.s.CacheHit++
	}
	if m.NoExec {
		s.s.NoExec++
	}
	if m.IsRemote {
		s.s.Remote++
	}
	if m.IsLocal {
		s.s.Local++
	}
	if m.Fallback {
		s.s.LocalFallback++
	}
	if m.Err {
		s.s.Fail++
	}
	if pure {
		s.s.Pure++
	}
}

// Stats keeps statistics about the build, such as the number of total, skipped or remote actions.
type Stats struct {
	Done            int // completed actions, including skipped, failed
	Fail            int // failed actions
	Pure            int // pure actions
	Skipped         int // skipped actions, because they were still up-to-date
	NoExec          int // actions that was completed by handler without execute cmds e.g. stamp, copy
	FastDepsSuccess int // actions that ran successfully when we used deps from the deps cache
	FastDepsFailed  int // actions that failed when we used deps from the deps cache
	ScanDepsFailed  int // actions that scandeps failed
	CacheHit        int // actions for which we got a cache hit
	Local           int // locally executed actions
	Remote          int // remote executed actions
	LocalFallback   int // actions for which remote execution failed, and we did a local fallback
	Total           int // total actions that ran during this build
}

func (s *stats) stats() Stats {
	if s == nil {
		return Stats{}
	}
	s.mu.Lock()
	stats := s.s
	s.mu.Unlock()
	return stats
}
