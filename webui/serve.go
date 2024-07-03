// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package webui implements siso webui.
package webui

import (
	"cmp"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/template"
	"time"

	"infra/build/siso/build"
)

//go:embed *.html *.css
var content embed.FS

const DefaultItemsPerPage = 100

type outdirInfo struct {
	buildMetrics  []build.StepMetric
	buildDuration build.IntervalMetric
	stepMetrics   map[string]build.StepMetric
	lastStepID    string
	ruleCounts    map[string]int
	actionCounts  map[string]int
}

func loadOutdirInfo(outdirPath string) (*outdirInfo, error) {
	metricsPath := filepath.Join(outdirPath, "siso_metrics.json")

	f, err := os.Open(metricsPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read metrics: %w", err)
	}
	defer f.Close()

	outdirInfo := &outdirInfo{
		buildMetrics: []build.StepMetric{},
		stepMetrics:  make(map[string]build.StepMetric),
		ruleCounts:   make(map[string]int),
		actionCounts: make(map[string]int),
	}

	d := json.NewDecoder(f)
	for {
		var m build.StepMetric
		err := d.Decode(&m)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parse error in %s:%d: %w", metricsPath, d.InputOffset(), err)
		}
		if m.BuildID != "" {
			outdirInfo.buildMetrics = append(outdirInfo.buildMetrics, m)
			// The last build metric found has the actual build duration.
			outdirInfo.buildDuration = m.Duration
		} else if m.StepID != "" {
			outdirInfo.stepMetrics[m.StepID] = m
			outdirInfo.lastStepID = m.StepID
		} else {
			return nil, fmt.Errorf("unexpected metric found %v", m)
		}
	}

	for _, metric := range outdirInfo.stepMetrics {
		if metric.Action != "" {
			outdirInfo.actionCounts[metric.Action]++
		}
	}
	for _, metric := range outdirInfo.stepMetrics {
		if metric.Rule != "" {
			outdirInfo.ruleCounts[metric.Rule]++
		}
	}

	return outdirInfo, err
}

func Serve(version string, localDevelopment bool, port int, outdir string) int {
	renderView := func(wr io.Writer, tmpl *template.Template, data map[string]any) error {
		data["outdir"] = outdir
		data["versionID"] = version
		err := tmpl.ExecuteTemplate(wr, "base", data)
		if err != nil {
			return fmt.Errorf("failed to execute template: %w", err)
		}
		return nil
	}

	// Prepare templates.
	// TODO(b/349287453): don't recompile templates every time if using embedded fs.
	fs := fs.FS(content)
	if localDevelopment {
		fs = os.DirFS("webui/")
	}

	// Read metrics.
	// TODO(b/349287453): support multiple outdirs.
	metrics, err := loadOutdirInfo(outdir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load outdir: %v", err)
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/steps/", http.StatusTemporaryRedirect)
	})

	http.HandleFunc("POST /steps/{id}/recall/", func(w http.ResponseWriter, r *http.Request) {
		tmpl, err := template.ParseFiles(
			"webui/base.html",
			"webui/_recall.html",
		)
		if err != nil {
			fmt.Fprintf(w, "failed to parse templates: %s\n", err)
			return
		}

		metric, ok := metrics.stepMetrics[r.PathValue("id")]
		if !ok {
			http.NotFound(w, r)
			return
		}

		err = renderView(w, tmpl, map[string]any{
			"step_id":        metric.StepID,
			"digest":         metric.Digest,
			"project":        r.FormValue("project"),
			"reapi_instance": r.FormValue("reapi_instance"),
		})
		if err != nil {
			fmt.Fprintf(w, "failed to render view: %s\n", err)
		}
	})

	http.HandleFunc("/steps/{id}/", func(w http.ResponseWriter, r *http.Request) {
		templates := []string{"base.html", "_step.html"}
		tmpl, err := template.ParseFS(fs, templates...)
		if err != nil {
			fmt.Fprintf(w, "failed to parse templates: %s\n", err)
			return
		}

		metric, ok := metrics.stepMetrics[r.PathValue("id")]
		if !ok {
			http.NotFound(w, r)
			return
		}

		var asMap map[string]any
		asJSON, err := json.Marshal(metric)
		if err != nil {
			// TODO(b/349287453): proper error handling.
			fmt.Fprintf(w, "failed to marshal metrics: %v\n", err)
		}
		err = json.Unmarshal(asJSON, &asMap)
		if err != nil {
			// TODO(b/349287453): proper error handling.
			fmt.Fprintf(w, "failed to unmarshal metrics: %v\n", err)
		}

		err = renderView(w, tmpl, asMap)
		if err != nil {
			fmt.Fprintf(w, "failed to render view: %v\n", err)
		}
	})

	http.HandleFunc("/steps/", func(w http.ResponseWriter, r *http.Request) {
		templates := []string{"base.html", "_steps.html"}
		tmpl, err := template.New("steps").Funcs(template.FuncMap{
			"currentURLHasParam": func(key string, value string) bool {
				q := r.URL.Query()
				if values, ok := q[key]; ok {
					return slices.Contains(values, value)
				}
				return false
			},
			"divIntervalsScaled": func(a build.IntervalMetric, b build.IntervalMetric, scale int) float64 {
				return float64(a) / float64(b) * float64(scale)
			},
			"addIntervals": func(a build.IntervalMetric, b build.IntervalMetric) build.IntervalMetric {
				return a + b
			},
			"formatIntervalMetric": func(i build.IntervalMetric) string {
				d := time.Duration(i)
				hour := int(d.Hours())
				minute := int(d.Minutes()) % 60
				second := int(d.Seconds()) % 60
				milli := d.Milliseconds() % 1000
				return fmt.Sprintf("%02d:%02d:%02d.%03d", hour, minute, second, milli)
			},
		}).ParseFS(fs, templates...)
		if err != nil {
			fmt.Fprintf(w, "failed to parse templates: %s\n", err)
			return
		}

		actionsWanted := r.URL.Query()["action"]
		rulesWanted := r.URL.Query()["rule"]
		view := r.URL.Query().Get("view")
		outputSearch := r.URL.Query().Get("q")

		var filteredSteps []build.StepMetric
		switch view {
		case "criticalPath":
			// We assume the last step is on the critical path.
			// Build the critical path backwards then reverse it.
			critStepID := metrics.lastStepID
			for {
				if critStepID == "" {
					break
				}
				step := metrics.stepMetrics[critStepID]
				filteredSteps = append(filteredSteps, step)
				// TODO(b/349287453): add some sort of error to indicate if prev step was not found
				critStepID = step.PrevStepID
			}
			slices.Reverse(filteredSteps)
		default:
			for _, m := range metrics.stepMetrics {
				if len(actionsWanted) > 0 && !slices.Contains(actionsWanted, m.Action) {
					continue
				}
				if len(rulesWanted) > 0 && !slices.Contains(rulesWanted, m.Rule) {
					continue
				}
				if outputSearch != "" && !strings.Contains(m.Output, outputSearch) {
					continue
				}
				filteredSteps = append(filteredSteps, m)
			}
			slices.SortFunc(filteredSteps, func(a, b build.StepMetric) int {
				return cmp.Compare(a.Ready, b.Ready)
			})
		}

		itemsPerPage, err := strconv.Atoi(r.URL.Query().Get("items_per_page"))
		if err != nil {
			itemsPerPage = DefaultItemsPerPage
		}
		requestedPage, err := strconv.Atoi(r.URL.Query().Get("page"))
		if err != nil {
			requestedPage = 0
		}
		pageCount := len(filteredSteps) / itemsPerPage
		if len(filteredSteps)%itemsPerPage > 0 {
			pageCount++
		}

		pageFirst := 0
		pageLast := pageCount - 1
		pageIndex := max(0, min(requestedPage, pageLast))
		pageNext := min(pageIndex+1, pageLast)
		pagePrev := max(0, pageIndex-1)
		itemsFirst := pageIndex * itemsPerPage
		itemsLast := max(0, min(itemsFirst+itemsPerPage, len(filteredSteps)))
		subset := filteredSteps[itemsFirst:itemsLast]

		data := map[string]any{
			"subset":             subset,
			"output_search":      outputSearch,
			"page":               requestedPage,
			"page_index":         pageIndex,
			"page_first":         pageFirst,
			"page_next":          pageNext,
			"page_prev":          pagePrev,
			"page_last":          pageLast,
			"page_count":         pageCount,
			"item_first_logical": itemsFirst + 1,
			"item_last_logical":  itemsFirst + len(subset),
			"items_len":          len(filteredSteps),
			"action_counts":      metrics.actionCounts,
			"rule_counts":        metrics.ruleCounts,
			"build_duration":     metrics.buildDuration,
		}
		err = renderView(w, tmpl, data)
		if err != nil {
			fmt.Fprintf(w, "failed to render view: %s\n", err)
		}
	})

	http.HandleFunc("/style.css", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, fs, "style.css")
	})

	fmt.Printf("listening on http://localhost:%d/...\n", port)
	err = http.ListenAndServe(fmt.Sprintf(":%d", port), nil)
	if errors.Is(err, http.ErrServerClosed) {
		fmt.Printf("server closed\n")
	} else if err != nil {
		fmt.Printf("error starting server: %s\n", err)
		return 1
	}
	return 0
}
