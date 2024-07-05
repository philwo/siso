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
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/template"
	"time"

	"infra/build/siso/build"
)

//go:embed *.html css/*.css
var content embed.FS

const DefaultItemsPerPage = 100

var (
	templates     = make(map[string]*template.Template)
	baseFunctions = template.FuncMap{
		"urlPathEq": func(url *url.URL, path string) bool {
			return url.Path == path
		},
		"urlPathHasPrefix": func(url *url.URL, prefix string) bool {
			return strings.HasPrefix(url.Path, prefix)
		},
		"urlHasParam": func(url *url.URL, key, value string) bool {
			return slices.Contains(url.Query()[key], value)
		},
		"divIntervalsScaled": func(a, b build.IntervalMetric, scale float64) float64 {
			return float64(a) / float64(b) * scale
		},
		"addIntervals": func(a, b build.IntervalMetric) build.IntervalMetric {
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
	}
)

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

// loadView lazy-parses a view once, or parses every time if in local development mode.
func loadView(localDevelopment bool, fs fs.FS, view string) (*template.Template, error) {
	if template, ok := templates[view]; ok {
		return template, nil
	}
	template, err := template.New("").Funcs(baseFunctions).ParseFS(fs, "base.html", view)
	if err != nil {
		return nil, fmt.Errorf("failed to parse view: %w", err)
	}
	if !localDevelopment {
		templates[view] = template
	}
	return template, nil
}

// Serve serves the webui.
func Serve(version string, localDevelopment bool, port int, outdir string) int {
	renderView := func(wr io.Writer, r *http.Request, tmpl *template.Template, data map[string]any) error {
		data["outdir"] = outdir
		data["versionID"] = version
		data["currentURL"] = r.URL
		err := tmpl.ExecuteTemplate(wr, "base", data)
		if err != nil {
			return fmt.Errorf("failed to execute template: %w", err)
		}
		return nil
	}

	// Use templates from embed or local.
	fs := fs.FS(content)
	if localDevelopment {
		fs = os.DirFS("webui/")
	}

	// Read metrics.
	// TODO(b/349287453): support multiple outdirs.
	metrics, err := loadOutdirInfo(outdir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load outdir: %v", err)
		return 1
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/steps/", http.StatusTemporaryRedirect)
	})

	http.HandleFunc("/logs/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/logs/.siso_config", http.StatusTemporaryRedirect)
	})

	http.HandleFunc("/logs/{file}", func(w http.ResponseWriter, r *http.Request) {
		tmpl, err := loadView(localDevelopment, fs, "_logs.html")
		if err != nil {
			// TODO(b/349287453): proper error handling.
			fmt.Fprintf(w, "failed to load view: %s\n", err)
			return
		}

		allowedFiles := []string{
			".siso_config",
			".siso_filegroups",
			"siso_output",
			"siso_localexec",
		}
		file := r.PathValue("file")

		if !slices.Contains(allowedFiles, file) {
			// TODO(b/349287453): proper error handling.
			fmt.Fprintf(w, "unknown file: %v\n", err)
			return
		}

		fileContents, err := os.ReadFile(path.Join(outdir, file))
		if err != nil {
			// TODO(b/349287453): proper error handling.
			fmt.Fprintf(w, "failed to open file: %s\n", err)
			return
		}

		err = renderView(w, r, tmpl, map[string]any{
			"allowedFiles": allowedFiles,
			"file":         file,
			"fileContents": string(fileContents),
		})
		if err != nil {
			// TODO(b/349287453): proper error handling.
			fmt.Fprintf(w, "failed to render view: %v\n", err)
		}
	})

	http.HandleFunc("POST /steps/{id}/recall/", func(w http.ResponseWriter, r *http.Request) {
		tmpl, err := loadView(localDevelopment, fs, "_recall.html")
		if err != nil {
			// TODO(b/349287453): proper error handling.
			fmt.Fprintf(w, "failed to load view: %s\n", err)
			return
		}

		metric, ok := metrics.stepMetrics[r.PathValue("id")]
		if !ok {
			http.NotFound(w, r)
			return
		}

		err = renderView(w, r, tmpl, map[string]any{
			"stepID":        metric.StepID,
			"digest":        metric.Digest,
			"project":       r.FormValue("project"),
			"reapiInstance": r.FormValue("reapi_instance"),
		})
		if err != nil {
			fmt.Fprintf(w, "failed to render view: %s\n", err)
		}
	})

	http.HandleFunc("/steps/{id}/", func(w http.ResponseWriter, r *http.Request) {
		tmpl, err := loadView(localDevelopment, fs, "_step.html")
		if err != nil {
			// TODO(b/349287453): proper error handling.
			fmt.Fprintf(w, "failed to load view: %s\n", err)
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

		err = renderView(w, r, tmpl, asMap)
		if err != nil {
			fmt.Fprintf(w, "failed to render view: %v\n", err)
		}
	})

	http.HandleFunc("/steps/", func(w http.ResponseWriter, r *http.Request) {
		tmpl, err := loadView(localDevelopment, fs, "_steps.html")
		if err != nil {
			// TODO(b/349287453): proper error handling.
			fmt.Fprintf(w, "failed to load view: %s\n", err)
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
			"subset":           subset,
			"outputSearch":     outputSearch,
			"page":             requestedPage,
			"pageIndex":        pageIndex,
			"pageFirst":        pageFirst,
			"pageNext":         pageNext,
			"pagePrev":         pagePrev,
			"pageLast":         pageLast,
			"pageCount":        pageCount,
			"itemFirstLogical": itemsFirst + 1,
			"itemLastLogical":  itemsFirst + len(subset),
			"itemsLen":         len(filteredSteps),
			"actionCounts":     metrics.actionCounts,
			"ruleCounts":       metrics.ruleCounts,
			"buildDuration":    metrics.buildDuration,
		}
		err = renderView(w, r, tmpl, data)
		if err != nil {
			fmt.Fprintf(w, "failed to render view: %s\n", err)
		}
	})

	http.Handle("/css/", http.FileServerFS(fs))

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
