// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"container/heap"
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/log"
	"go.chromium.org/infra/build/siso/execute"
	"go.chromium.org/infra/build/siso/ui"
)

type progress struct {
	started    time.Time
	numLocal   atomic.Int32
	mu         sync.Mutex
	consoleCmd *execute.Cmd

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

func (p *progress) startConsoleCmd(cmd *execute.Cmd) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cmd.ConsoleOut = new(atomic.Bool)
	p.consoleCmd = cmd
}

func (p *progress) finishConsoleCmd() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.consoleCmd = nil
}

func (p *progress) update(ctx context.Context, b *Builder) {
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
			consoleOut := p.consoleCmd != nil && p.consoleCmd.ConsoleOut != nil && p.consoleCmd.ConsoleOut.Load()
			p.mu.Unlock()
			if consoleOut {
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
			phase := si.step.phase()
			dur := si.step.servDuration()
			var msg string
			if dur > 0 {
				msg = fmt.Sprintf("%s[%s]: %s", ui.FormatDuration(dur), phase, si.desc)
			} else {
				msg = fmt.Sprintf("[%s]: %s", phase, si.desc)
			}
			switch phase {
			case stepRetryWait, stepRetryRun:
				msg = ui.SGR(ui.Red, msg)
			}
			p.step(b, si.step, msg)
		}
	}
}

func (p *progress) stop() {
	close(p.done)
	<-p.updateStopped
}

func (p *progress) report(format string, args ...any) {
	var msg string
	if msg == "" {
		msg = fmt.Sprintf(format, args...)
	}
	log.Info(msg)
}

const (
	progressPrefixCacheHit = "c "
	progressPrefixStart    = "S "
	progressPrefixFinish   = "F "
	progressPrefixRetry    = "r "
)

func (p *progress) step(b *Builder, step *Step, s string) {
	if step == nil {
		log.Info(s)
		return
	}

	if strings.HasPrefix(s, progressPrefixStart) {
		p.mu.Lock()
		heap.Push(&p.actives, &stepInfo{
			step: step,
			desc: step.cmd.Desc,
		})
		p.mu.Unlock()
	}

	if strings.HasPrefix(s, progressPrefixCacheHit) || strings.HasPrefix(s, progressPrefixFinish) {
		dur := ui.FormatDuration(time.Since(b.start))
		stat := b.stats.stats()
		msg := fmt.Sprintf("[%d/%d] %s ",
			stat.Done-stat.Skipped, stat.Total-stat.Skipped, dur)
		msg += s[len(progressPrefixFinish):]

		log.Info(msg)

		outputResult := step.cmd.OutputResult()
		if outputResult != "" {
			log.Info(outputResult)
		}
	}
}

type ActiveStepInfo struct {
	ID    string
	Desc  string
	Phase string

	// step duration since when it's ready to run.
	Dur string

	// step accumulated service duration (local exec, or remote call).
	// not including local execution queue, remote execution queue,
	// but including all of remote call (reapi), uploading
	// inputs, downloading outputs etc.
	ServDur string
}

func (p *progress) ActiveSteps() []ActiveStepInfo {
	p.mu.Lock()
	actives := make([]*stepInfo, len(p.actives))
	copy(actives, p.actives)
	p.mu.Unlock()
	now := time.Now()
	var as []*stepInfo
	for _, s := range actives {
		phase := s.step.phase().String()
		if phase == "done" {
			continue
		}
		as = append(as, s)
	}
	sort.Slice(as, func(i, j int) bool {
		return as[i].step.startTime.Before(as[j].step.startTime)
	})

	activeSteps := make([]ActiveStepInfo, 0, len(as))
	for _, s := range as {
		phase := s.step.phase().String()
		if phase == "done" {
			continue
		}
		var servDur string
		if dur := s.step.servDuration(); dur > 0 {
			servDur = ui.FormatDuration(dur)
		}
		activeSteps = append(activeSteps, ActiveStepInfo{
			ID:      s.step.String(),
			Desc:    s.desc,
			Phase:   phase,
			Dur:     ui.FormatDuration(now.Sub(s.step.startTime)),
			ServDur: servDur,
		})
	}
	return activeSteps
}
