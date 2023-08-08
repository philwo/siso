// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"container/heap"
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"infra/build/siso/ui"
)

type progress struct {
	started time.Time
	mu      sync.Mutex
	ts      time.Time

	actives       activeSteps
	done          chan struct{}
	updateStopped chan struct{}
	count         atomic.Int64
}

type stepInfo struct {
	step *Step
	desc string
}

type activeSteps []*stepInfo

func (as activeSteps) Len() int           { return len(as) }
func (as activeSteps) Less(i, j int) bool { return as[i].step.startTime.Before(as[j].step.startTime) }
func (as activeSteps) Swap(i, j int)      { as[i], as[j] = as[j], as[i] }
func (as *activeSteps) Push(x any) {
	(*as) = append(*as, x.(*stepInfo))
}
func (as *activeSteps) Pop() any {
	old := *as
	n := len(old)
	s := old[n-1]
	old[n-1] = nil
	*as = old[:n-1]
	return s
}

func (p *progress) start(ctx context.Context, b *Builder) {
	p.started = time.Now()
	p.done = make(chan struct{})
	p.updateStopped = make(chan struct{})
	go p.update(ctx, b)
}

func (p *progress) update(ctx context.Context, b *Builder) {
	lastUpdate := time.Now()
	lastStepUpdate := time.Now()
	defer close(p.updateStopped)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-p.done:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.count.Add(1)
			p.mu.Lock()
			var si *stepInfo
			for len(p.actives) > 0 {
				s := p.actives[0]
				if s.step.Done() {
					// already finished?
					heap.Pop(&p.actives)
					continue
				}
				si = s
				break
			}
			if len(p.actives) > 0 {
				d := time.Since(lastUpdate)
				wd := d / time.Duration(len(p.actives))
				lastUpdate = time.Now()
				for _, s := range p.actives {
					s.step.addWeightedDuration(wd)
				}
			}
			p.mu.Unlock()
			if !ui.IsTerminal() {
				continue
			}
			if si == nil || si.step == nil {
				continue
			}
			const d = 100 * time.Millisecond
			if time.Since(lastStepUpdate) < d {
				continue
			}
			lastStepUpdate = time.Now()
			dur := time.Since(si.step.startTime).Round(d)
			p.step(ctx, b, si.step, fmt.Sprintf("%s[%s]: %s", ui.FormatDuration(dur), si.step.phase(), si.desc))
		}
	}
}

func (p *progress) stop(ctx context.Context) {
	close(p.done)
	<-p.updateStopped
}

func (p *progress) report(format string, args ...any) {
	p.mu.Lock()
	t := p.ts
	p.mu.Unlock()
	if ui.IsTerminal() && time.Since(t) < 500*time.Millisecond {
		return
	}
	msg := fmt.Sprintf(format, args...)
	ui.Default.PrintLines(msg)
	p.mu.Lock()
	p.ts = time.Now()
	p.mu.Unlock()
}

const (
	progressPrefixStart  = "S "
	progressPrefixFinish = "F "
)

func (p *progress) step(ctx context.Context, b *Builder, step *Step, s string) {
	p.mu.Lock()
	t := p.ts
	if step != nil {
		if strings.HasPrefix(s, progressPrefixStart) {
			heap.Push(&p.actives, &stepInfo{
				step: step,
				desc: step.cmd.Desc,
			})
		}
	}
	p.mu.Unlock()
	if ui.IsTerminal() && (time.Since(t) < 30*time.Millisecond || strings.HasPrefix(s, progressPrefixFinish)) {
		return
	}
	stat := b.stats.stats()
	var lines []string
	if ui.IsTerminal() {
		runProgress := func(waits, servs int) string {
			if waits > 0 {
				return ui.SGR(ui.BackgroundRed, fmt.Sprintf("%d", waits+servs))
			}
			return fmt.Sprintf("%d", servs)
		}

		localProgress := runProgress(b.localSema.NumWaits(), b.localSema.NumServs())
		remoteProgress := runProgress(b.reproxySema.NumWaits()+b.rewrapSema.NumWaits()+b.remoteSema.NumWaits(), b.reproxySema.NumServs()+b.rewrapSema.NumServs()+b.remoteSema.NumServs())
		var stepsPerSec string
		if stat.Done > 0 {
			stepsPerSec = fmt.Sprintf("%.1f/s ", float64(stat.Done)/time.Since(p.started).Seconds())
		}
		var cacheHitRatio string
		if stat.Remote+stat.CacheHit > 0 {
			cacheHitRatio = fmt.Sprintf("cache:%5.02f%% ", float64(stat.CacheHit)/float64(stat.CacheHit+stat.Remote)*100.0)
		}
		var fallback string
		if stat.LocalFallback > 0 {
			fallback = ui.SGR(ui.BackgroundRed, fmt.Sprintf("%d", stat.LocalFallback))
		} else {
			fallback = "0"
		}
		lines = append(lines, fmt.Sprintf("pre:%d local:%s remote:%s %s%sfallback:%s",
			stat.Preproc,
			localProgress,
			remoteProgress,
			stepsPerSec,
			cacheHitRatio,
			fallback))
	}
	msg := s
	if step != nil {
		msg = fmt.Sprintf("%.2f%% %s %s",
			float64(stat.Done)*100/float64(stat.Total),
			ui.FormatDuration(time.Since(b.start)),
			s)
	}
	lines = append(lines, msg)
	ui.Default.PrintLines(lines...)
	p.mu.Lock()
	p.ts = time.Now()
	p.mu.Unlock()
}
