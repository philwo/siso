// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
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

func (s *stats) update(skipped bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.s.Done++
	if skipped {
		s.s.Skipped++
	}
}

// Stats keeps statistics about the build, such as the number of total, skipped or remote actions.
type Stats struct {
	Done    int // completed actions, including skipped, failed
	Skipped int // skipped actions, because they were still up-to-date
	Total   int // total actions that ran during this build
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
