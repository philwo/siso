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
	verbose    bool
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
	p.verbose = b.verbose
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
			consoleOut := p.consoleCmd != nil && p.consoleCmd.ConsoleOut != nil && p.consoleCmd.ConsoleOut.Load()
			p.mu.Unlock()
			if !ui.IsTerminal() || b.verbose || consoleOut {
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
			case stepFallbackWait, stepFallbackRun, stepRetryWait, stepRetryRun:
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
	ui.Default.Infof(format, args...)
}

const (
	progressPrefixCacheHit = "c "
	progressPrefixStart    = "S "
	progressPrefixFinish   = "F "
	progressPrefixRetry    = "r "
	progressPrefixFallback = "f "
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
		if p.verbose {
			msg += step.def.Binding("command")
		} else {
			msg += s[len(progressPrefixFinish):]
		}

		ui.Default.Infof(msg)

		outputResult := step.cmd.OutputResult()
		if outputResult != "" {
			ui.Default.Infof(outputResult)
		}
	}

	// case ui.IsTerminal():
	// 	runProgress := func(waits, servs int) string {
	// 		if waits > 0 {
	// 			return ui.SGR(ui.BackgroundRed, fmt.Sprintf("%d", waits+servs))
	// 		}
	// 		return fmt.Sprintf("%d", servs)
	// 	}
	// 	preprocWaits := b.preprocSema.NumWaits()
	// 	preprocServs := b.preprocSema.NumServs()
	// 	preprocProgress := runProgress(preprocWaits, preprocServs)
	//
	// 	localWaits := b.localSema.NumWaits()
	// 	localServs := b.localSema.NumServs()
	// 	for _, p := range b.poolSemas {
	// 		localWaits += p.NumWaits()
	// 		localServs += p.NumServs()
	// 	}
	// 	p.numLocal.Store(int32(localWaits + localServs))
	// 	localProgress := runProgress(localWaits, localServs)
	//
	// 	remoteWaits := b.remoteSema.NumWaits()
	// 	remoteServs := b.remoteSema.NumServs()
	// 	remoteWaits += b.rewrapSema.NumWaits()
	// 	remoteServs += b.rewrapSema.NumServs()
	// 	remoteProgress := runProgress(remoteWaits, remoteServs)
	//
	// 	var stepsPerSec string
	// 	if stat.Done-stat.Skipped > 0 {
	// 		stepsPerSec = fmt.Sprintf("%.1f/s ", float64(stat.Done-stat.Skipped)/time.Since(p.started).Seconds())
	// 	}
	// 	var cacheHitRatio string
	// 	if stat.Remote+stat.CacheHit > 0 {
	// 		cacheHitRatio = fmt.Sprintf("cache:%5.02f%% ", float64(stat.CacheHit)/float64(stat.CacheHit+stat.Remote)*100.0)
	// 	}
	// 	var fallback string
	// 	if stat.LocalFallback > 0 {
	// 		fallback = "fallback:" + ui.SGR(ui.BackgroundRed, fmt.Sprintf("%d", stat.LocalFallback)) + " "
	// 	}
	// 	var retry string
	// 	if stat.RemoteRetry > 0 {
	// 		retry = "retry:" + ui.SGR(ui.BackgroundRed, fmt.Sprintf("%d", stat.RemoteRetry)) + " "
	// 	}
	// 	if outputResult == "" {
	// 		lines = append(lines, fmt.Sprintf("pre:%s local:%s remote:%s %s%s%s%s",
	// 			preprocProgress,
	// 			localProgress,
	// 			remoteProgress,
	// 			stepsPerSec,
	// 			cacheHitRatio,
	// 			fallback,
	// 			retry,
	// 		))
	// 	}
	// 	fallthrough
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
