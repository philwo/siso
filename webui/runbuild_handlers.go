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
	"sync"
	"time"

	"infra/build/siso/ui"
)

var ninjaStepRe = regexp.MustCompile(`\[(?P<stepNum>[0-9]+?)/(?P<totalSteps>[0-9]+?)\] (?P<time>[^\s]+?) (?P<status>[SF]) (?P<type>[^\s]+?) (?P<out>.+)`)

// runBuildRouter returns *http.ServeMux with handlers related to running builds registered at the root.
// The consumer of this function is responsible for nesting this *http.ServeMux appropriately.
// All handlers assume request's context.Context contains outroot, outsub.
func (s *WebuiServer) runBuildRouter(sseServer *sseServer) *http.ServeMux {
	runBuildRouter := http.NewServeMux()

	runBuildRouter.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
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

		err = s.renderBuildView(w, r, tmpl, outdirInfo, map[string]any{})
		if err != nil {
			s.renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to render view: %v", err), w, r, outdirInfo)
		}
	})

	runBuildRouter.HandleFunc("POST /", func(w http.ResponseWriter, r *http.Request) {
		outdirInfo, err := s.ensureOutdirForRequest(r)
		if err != nil {
			s.renderBuildViewError(http.StatusNotFound, fmt.Sprintf("outdir failed to load for request %s: %v", r.URL, err), w, r, outdirInfo)
			return
		}

		exe, err := os.Executable()
		if err != nil {
			s.renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to detect siso path: %v", err), w, r, outdirInfo)
		}

		tmpl, err := s.loadView("_error.html")
		if err != nil {
			s.renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to load view: %s", err), w, r, outdirInfo)
			return
		}

		cmd := exec.Command(exe, "ninja", "-C", "out/Default", "base") // outdirInfo.path
		cmd.Dir = s.execRoot
		pipe, _ := cmd.StdoutPipe()
		cmd.Stderr = cmd.Stdout
		if err := cmd.Start(); err != nil {
			s.renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to launch process: %s", err), w, r, outdirInfo)
			return
		}
		done := make(chan error)
		go func() {
			done <- cmd.Wait()
		}()

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

					sseServer.messages <- sseMessage{"buildstatus", fmt.Sprintf("<li>%d active steps<li>%d/%s steps done", len(activeSteps), maxStep, totalSteps)}
				} else {
					sseServer.messages <- sseMessage{"buildlog", fmt.Sprintf("<div>%s</div>", line)}
				}
				line, err = reader.ReadString('\n')
			}
			fmt.Fprintf(os.Stderr, "A build was finished\n")
		}(pipe)
		go func() {
			for {
				select {
				case <-done:
					sseServer.messages <- sseMessage{"activesteps", "Finished"}
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
					sseServer.messages <- sseMessage{"activesteps", b.String()}
				}
			}
		}()

		err = s.renderBuildView(w, r, tmpl, outdirInfo, map[string]any{
			"errorTitle":   "Success",
			"errorMessage": "Launched build",
		})
		if err != nil {
			s.renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to render view: %v", err), w, r, outdirInfo)
		}
	})

	return runBuildRouter
}
