// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"sync/atomic"

	log "github.com/golang/glog"

	"infra/build/siso/o11y/clog"
)

type stats struct {
	ntotal           atomic.Int32
	nskipped         atomic.Int32
	nfastDepsSuccess atomic.Int32
	nfastDepsFailed  atomic.Int32
	npreproc         atomic.Int32
	ncacheHit        atomic.Int32
	nlocal           atomic.Int32
	nremote          atomic.Int32
	nlocalFallback   atomic.Int32
	ndone            atomic.Int32
	npure            atomic.Int32
}

func (s *stats) skipped(ctx context.Context) int {
	if log.V(1) {
		clog.Infof(ctx, "step state: skip")
	}
	return int(s.nskipped.Add(1))
}

func (s *stats) fastDepsSuccess(ctx context.Context) {
	if log.V(1) {
		clog.Infof(ctx, "fast deps success")
	}
	s.nfastDepsSuccess.Add(1)
}

func (s *stats) fastDepsFailed(ctx context.Context, err error) {
	clog.Warningf(ctx, "fast deps failed: %v", err)
	s.nfastDepsFailed.Add(1)
}

func (s *stats) preprocStart(ctx context.Context) {
	if log.V(1) {
		clog.Infof(ctx, "step state: preproc start")
	}
	s.npreproc.Add(1)
}

func (s *stats) preprocEnd(ctx context.Context) {
	if log.V(1) {
		clog.Infof(ctx, "step state: preproc end")
	}
	s.npreproc.Add(-1)
}

func (s *stats) cacheHit(ctx context.Context) {
	clog.Infof(ctx, "step state: cache hit")
	s.ncacheHit.Add(1)
}

func (s *stats) remoteDone(ctx context.Context, err error) {
	clog.Infof(ctx, "step state: remote done: %v", err)
	s.nremote.Add(1)
}

func (s *stats) localFallback(ctx context.Context) {
	clog.Warningf(ctx, "step state: localfallback")
	s.nlocalFallback.Add(1)
}

func (s *stats) localDone(ctx context.Context, err error) {
	clog.Infof(ctx, "step state: local done: %v", err)
	s.nlocal.Add(1)
}

func (s *stats) done(pure bool) {
	s.ndone.Add(1)
	if pure {
		s.npure.Add(1)
	}
}

func (s *stats) incTotal() {
	s.ntotal.Add(1)
}

// Stats keeps statistics about the build, such as the number of total, skipped or remote actions.
type Stats struct {
	Preproc         int // preprocessor actions
	Done            int // completed actions
	Pure            int // pure actions
	Skipped         int // skipped actions, because they were still up-to-date
	FastDepsSuccess int // actions that ran successfully when we used deps from the deps cache
	FastDepsFailed  int // actions that failed when we used deps from the deps cache
	CacheHit        int // actions for which we got a cache hit
	Local           int // locally executed actions
	Remote          int // remote executed actions
	LocalFallback   int // actions for which remote execution failed, and we did a local fallback
	Total           int // total actions that ran during this build
}

func (s *stats) stats() Stats {
	return Stats{
		Preproc:         int(s.npreproc.Load()),
		Done:            int(s.ndone.Load()),
		Pure:            int(s.npure.Load()),
		Skipped:         int(s.nskipped.Load()),
		FastDepsSuccess: int(s.nfastDepsSuccess.Load()),
		FastDepsFailed:  int(s.nfastDepsFailed.Load()),
		CacheHit:        int(s.ncacheHit.Load()),
		Local:           int(s.nlocal.Load()),
		Remote:          int(s.nremote.Load()),
		LocalFallback:   int(s.nlocalFallback.Load()),
		Total:           int(s.ntotal.Load()),
	}
}
