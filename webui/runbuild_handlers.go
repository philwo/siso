// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package webui

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"infra/build/siso/ui"
)

var ninjaStepRe = regexp.MustCompile(`\[(?P<stepNum>[0-9]+?)/(?P<totalSteps>[0-9]+?)\] (?P<time>[^\s]+?) (?P<status>[SF]) (?P<type>[^\s]+?) (?P<out>.+)`)

type runbuildState struct {
	activeBuildMu      sync.Mutex
	activeBuildLog     strings.Builder
	activeBuildRunning bool
	activeBuildOutdir  string
	activeBuildTarget  string
}

func (s *WebuiServer) handleRunbuildGet(w http.ResponseWriter, r *http.Request) {
	outdirInfo, err := s.ensureOutdirForRequest(r)
	if err != nil {
		s.renderBuildViewError(http.StatusNotFound, fmt.Sprintf("outdir failed to load for request %s: %v", r.URL, err), w, r, outdirInfo)
		return
	}

	tmpl, err := s.loadView("_run.html")
	if err != nil {
		s.renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to load view: %s", err), w, r, outdirInfo)
		return
	}

	s.activeBuildMu.Lock()
	defer s.activeBuildMu.Unlock()
	err = s.renderBuildView(w, r, tmpl, outdirInfo, map[string]any{
		"activeBuildLog":     s.activeBuildLog.String(),
		"activeBuildRunning": s.activeBuildRunning,
		"activeBuildOutdir":  s.activeBuildOutdir,
		"activeBuildTarget":  s.activeBuildTarget,
	})
	if err != nil {
		s.renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to render view: %v", err), w, r, outdirInfo)
	}
}

func (s *WebuiServer) handleRunbuildPost(w http.ResponseWriter, r *http.Request) {
	outdirInfo, err := s.ensureOutdirForRequest(r)
	if err != nil {
		s.renderBuildViewError(http.StatusNotFound, fmt.Sprintf("outdir failed to load for request %s: %v", r.URL, err), w, r, outdirInfo)
		return
	}

	exe, err := os.Executable()
	if err != nil {
		s.renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to detect siso path: %v", err), w, r, outdirInfo)
	}

	// We'll render the same view again, but in a "building" state.
	tmpl, err := s.loadView("_run.html")
	if err != nil {
		s.renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to load view: %s", err), w, r, outdirInfo)
		return
	}

	s.activeBuildMu.Lock()
	defer s.activeBuildMu.Unlock()
	if s.activeBuildRunning {
		s.renderBuildViewError(http.StatusServiceUnavailable, "Existing build already running", w, r, outdirInfo)
		return
	}

	s.activeBuildRunning = true
	s.activeBuildLog.Reset()
	s.activeBuildOutdir = r.FormValue("outdir")
	s.activeBuildTarget = r.FormValue("target")
	cmd := exec.Command(exe, "ninja", "-C", s.activeBuildOutdir, s.activeBuildTarget)
	cmd.Dir = s.execRoot
	pipe, _ := cmd.StdoutPipe()
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		s.renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to launch process: %s", err), w, r, outdirInfo)
		return
	}

	activeSteps := make(map[string]runningStepInfo)
	activeStepsLock := &sync.RWMutex{}
	var maxStep int64

	go func(p io.ReadCloser) {
		reader := bufio.NewReader(pipe)
		line, err := reader.ReadString('\n')
		for err == nil {
			if ninjaStepRe.MatchString(line) {
				matches := ninjaStepRe.FindStringSubmatch(line)
				stepNum := string(matches[ninjaStepRe.SubexpIndex("stepNum")])
				totalSteps := string(matches[ninjaStepRe.SubexpIndex("totalSteps")])
				// stepTime := string(matches[ninjaStepRe.SubexpIndex("time")])
				status := string(matches[ninjaStepRe.SubexpIndex("status")])
				stepType := string(matches[ninjaStepRe.SubexpIndex("type")])
				stepOut := string(matches[ninjaStepRe.SubexpIndex("out")])

				stepNumParsed, err := strconv.ParseInt(stepNum, 0, 64)
				if err == nil && stepNumParsed > maxStep {
					maxStep = stepNumParsed
				}

				activeStepsLock.Lock()
				if status == "S" {
					activeSteps[stepOut] = runningStepInfo{
						stepOut:  stepOut,
						stepType: stepType,
						started:  time.Now(),
					}
				} else if status == "F" {
					delete(activeSteps, stepOut)
				}
				activeStepsLock.Unlock()

				s.sseServer.messages <- sseMessage{"buildstatus", fmt.Sprintf("<li>%d active steps<li>%d/%s steps done", len(activeSteps), maxStep, totalSteps)}
			} else {
				newLine := fmt.Sprintf("<div>%s</div>", line)
				s.activeBuildLog.WriteString(newLine)
				s.sseServer.messages <- sseMessage{"buildlog", newLine}
			}
			line, err = reader.ReadString('\n')
		}
		fmt.Fprintf(os.Stderr, "A build was finished\n")
	}(pipe)

	done := make(chan error)
	go func() {
		done <- cmd.Wait()
	}()
	go func() {
		for {
			select {
			case <-done:
				s.activeBuildMu.Lock()
				s.sseServer.messages <- sseMessage{"activesteps", "Finished"}
				s.activeBuildRunning = false
				s.activeBuildMu.Unlock()
				return
			case <-time.After(100 * time.Millisecond):
				activeStepsLock.RLock()
				// TODO: use golang 1.23 maps.Values
				activeByStarted := make([]runningStepInfo, 0, len(activeSteps))
				for _, value := range activeSteps {
					activeByStarted = append(activeByStarted, value)
				}
				activeStepsLock.RUnlock()

				slices.SortFunc(activeByStarted, func(a, b runningStepInfo) int {
					return a.started.Compare(b.started)
				})

				b := new(bytes.Buffer)
				fmt.Fprintf(b, "<table>")
				for _, stepInfo := range activeByStarted {
					fmt.Fprintf(
						b, "<tr><td>%s</td><td>%s</td><td>%s</td></tr>",
						stepInfo.stepType,
						ui.FormatDuration(time.Since(stepInfo.started)),
						stepInfo.stepOut,
					)
				}
				fmt.Fprintf(b, "</table>")
				s.sseServer.messages <- sseMessage{"activesteps", b.String()}
			}
		}
	}()

	err = s.renderBuildView(w, r, tmpl, outdirInfo, map[string]any{
		"activeBuildRunning": s.activeBuildRunning,
		"activeBuildOutdir":  s.activeBuildOutdir,
		"activeBuildTarget":  s.activeBuildTarget,
	})
	if err != nil {
		s.renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to render view: %v", err), w, r, outdirInfo)
	}
}
