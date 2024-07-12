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
	"path/filepath"
	"regexp"
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
	metrics []*buildMetrics
}

type buildMetrics struct {
	rev           string
	buildMetrics  []build.StepMetric
	buildDuration build.IntervalMetric
	stepMetrics   map[string]build.StepMetric
	lastStepID    string
	ruleCounts    map[string]int
	actionCounts  map[string]int
}

func loadBuildMetrics(metricsPath string) (*buildMetrics, error) {
	f, err := os.Open(metricsPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read metrics: %w", err)
	}
	defer f.Close()

	metricsData := &buildMetrics{
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
			metricsData.buildMetrics = append(metricsData.buildMetrics, m)
			// The last build metric found has the actual build duration.
			metricsData.buildDuration = m.Duration
		} else if m.StepID != "" {
			metricsData.stepMetrics[m.StepID] = m
			metricsData.lastStepID = m.StepID
		} else {
			return nil, fmt.Errorf("unexpected metric found %v", m)
		}
	}

	for _, metric := range metricsData.stepMetrics {
		if metric.Action != "" {
			metricsData.actionCounts[metric.Action]++
		}
	}
	for _, metric := range metricsData.stepMetrics {
		if metric.Rule != "" {
			metricsData.ruleCounts[metric.Rule]++
		}
	}

	return metricsData, nil
}

// loadOutdirInfo attempts to load all metrics found in the outdir.
func loadOutdirInfo(outdirPath string) (*outdirInfo, error) {
	outdirInfo := &outdirInfo{}

	// Attempt to load latest metrics first.
	// Only silently ignore if it doesn't exist, otherwise always return error.
	// TODO(b/349287453): consider tolerate fail, so frontend can show error?
	latestMetricsPath := filepath.Join(outdirPath, "siso_metrics.json")
	_, err := os.Stat(latestMetricsPath)
	if err == nil {
		latestMetrics, err := loadBuildMetrics(latestMetricsPath)
		if err != nil {
			return nil, fmt.Errorf("failed to load latest metrics: %w", err)
		}
		latestMetrics.rev = "latest"
		outdirInfo.metrics = append(outdirInfo.metrics, latestMetrics)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("failed to stat latest metrics: %w", err)
	}

	// Then load revisions if available.
	// Always return error if loading any fails.
	// TODO(b/349287453): consider tolerate fail, so frontend can show error?
	revPaths, err := filepath.Glob(filepath.Join(outdirPath, "siso_metrics.*.json"))
	if err != nil {
		return nil, fmt.Errorf("failed to glob revs: %w", err)
	}
	for _, revPath := range revPaths {
		baseName := filepath.Base(revPath)
		re := regexp.MustCompile(`siso_metrics.(\d+).json`)
		matches := re.FindStringSubmatch(baseName)
		if matches == nil {
			fmt.Fprintf(os.Stderr, "ignoring invalid %s\n", revPath)
		}
		rev := matches[1]

		revMetrics, err := loadBuildMetrics(revPath)
		if err != nil {
			return nil, fmt.Errorf("failed to load %s: %w", revPath, err)
		}
		revMetrics.rev = rev
		outdirInfo.metrics = append(outdirInfo.metrics, revMetrics)
	}
	return outdirInfo, nil
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
func Serve(version string, localDevelopment bool, port int, defaultOutdir, configRepoDir string) int {
	// Use templates from embed or local.
	fs := fs.FS(content)
	if localDevelopment {
		fs = os.DirFS("webui/")
	}

	// Read metrics.
	// TODO(b/349287453): support multiple outdirs.
	outdirInfo, err := loadOutdirInfo(defaultOutdir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load outdir: %v", err)
		return 1
	}
	var knownRevs []string
	for _, metrics := range outdirInfo.metrics {
		knownRevs = append(knownRevs, metrics.rev)
	}

	// Find other outdirs.
	// TODO(b/349287453): use list to implement outdir switching.
	execRoot, err := build.DetectExecRoot(defaultOutdir, configRepoDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to find execroot: %v", err)
		return 1
	}
	matches, err := filepath.Glob(filepath.Join(execRoot, "out/*"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to glob %s: %v", execRoot, err)
		return 1
	}
	var outdirs []string
	for _, match := range matches {
		m, err := os.Stat(match)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to stat outdir %s: %v", match, err)
			return 1
		}
		if m.IsDir() {
			outdirs = append(outdirs, match)
		}
	}

	renderBuildView := func(wr io.Writer, r *http.Request, tmpl *template.Template, data map[string]any) error {
		rev := r.PathValue("rev")
		outdirShort := defaultOutdir
		if home, err := os.UserHomeDir(); err == nil {
			outdirShort = strings.Replace(outdirShort, home, "~", 1)
		}
		data["prefix"] = fmt.Sprintf("/builds/%s", rev)
		data["outdirShort"] = outdirShort
		data["outdirs"] = outdirs
		data["versionID"] = version
		data["currentURL"] = r.URL
		data["currentRev"] = rev
		data["revs"] = knownRevs
		err := tmpl.ExecuteTemplate(wr, "base", data)
		if err != nil {
			return fmt.Errorf("failed to execute template: %w", err)
		}
		return nil
	}

	http.HandleFunc("/builds/{rev}/logs/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, fmt.Sprintf("/builds/%s/logs/.siso_config", r.PathValue("rev")), http.StatusTemporaryRedirect)
	})

	http.HandleFunc("/builds/{rev}/logs/{file}", func(w http.ResponseWriter, r *http.Request) {
		tmpl, err := loadView(localDevelopment, fs, "_logs.html")
		if err != nil {
			// TODO(b/349287453): proper error handling.
			fmt.Fprintf(w, "failed to load view: %s\n", err)
			return
		}

		allowedFilesMap := map[string]string{
			".siso_config":     ".siso_config%.0s",
			".siso_filegroups": ".siso_filegroups%.0s",
			"siso_localexec":   "siso_localexec.%s",
			"siso_output":      "siso_output.%s",
			"siso_trace.json":  "siso_trace.%s.json",
		}
		requestedFile := r.PathValue("file")
		revFileFormatter, ok := allowedFilesMap[requestedFile]
		if !ok {
			// TODO(b/349287453): proper error handling.
			fmt.Fprintf(w, "unknown file: %v\n", err)
			return
		}

		rev := r.PathValue("rev")
		actualFile := requestedFile
		if rev != "latest" {
			actualFile = fmt.Sprintf(revFileFormatter, rev)
		}
		fileContents, err := os.ReadFile(filepath.Join(defaultOutdir, actualFile))
		if err != nil {
			// TODO(b/349287453): proper error handling.
			fmt.Fprintf(w, "failed to open file: %s\n", err)
			return
		}

		if r.URL.Query().Get("raw") == "true" {
			w.Header().Add("Content-Type", "text/plain; charset=UTF-8")
			_, err := w.Write(fileContents)
			if err != nil {
				fmt.Fprintf(w, "failed to write: %v", err)
			}
			return
		}

		// TODO(b/349287453): use maps.Keys once go 1.23
		var allowedFiles []string
		for allowedFile := range allowedFilesMap {
			allowedFiles = append(allowedFiles, allowedFile)
		}
		slices.Sort(allowedFiles)

		err = renderBuildView(w, r, tmpl, map[string]any{
			"allowedFiles": allowedFiles,
			"file":         requestedFile,
			"fileContents": string(fileContents),
		})
		if err != nil {
			// TODO(b/349287453): proper error handling.
			fmt.Fprintf(w, "failed to render view: %v\n", err)
		}
	})

	http.HandleFunc("POST /builds/{rev}/steps/{id}/recall/", func(w http.ResponseWriter, r *http.Request) {
		var metrics *buildMetrics
		for _, m := range outdirInfo.metrics {
			if m.rev == r.PathValue("rev") {
				metrics = m
				break
			}
		}
		if metrics == nil {
			http.NotFound(w, r)
			return
		}

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

		err = renderBuildView(w, r, tmpl, map[string]any{
			"stepID":        metric.StepID,
			"digest":        metric.Digest,
			"project":       r.FormValue("project"),
			"reapiInstance": r.FormValue("reapi_instance"),
		})
		if err != nil {
			fmt.Fprintf(w, "failed to render view: %s\n", err)
		}
	})

	http.HandleFunc("/builds/{rev}/steps/{id}/", func(w http.ResponseWriter, r *http.Request) {
		var metrics *buildMetrics
		for _, m := range outdirInfo.metrics {
			if m.rev == r.PathValue("rev") {
				metrics = m
				break
			}
		}
		if metrics == nil {
			http.NotFound(w, r)
			return
		}

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

		err = renderBuildView(w, r, tmpl, asMap)
		if err != nil {
			fmt.Fprintf(w, "failed to render view: %v\n", err)
		}
	})

	http.HandleFunc("/builds/{rev}/steps/", func(w http.ResponseWriter, r *http.Request) {
		var metrics *buildMetrics
		for _, m := range outdirInfo.metrics {
			if m.rev == r.PathValue("rev") {
				metrics = m
				break
			}
		}
		if metrics == nil {
			http.NotFound(w, r)
			return
		}

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
		err = renderBuildView(w, r, tmpl, data)
		if err != nil {
			fmt.Fprintf(w, "failed to render view: %s\n", err)
		}
	})

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// "/" handler becomes a catch-all for requests that didn't match a pattern, so those need to return 404.
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/builds/latest/steps/", http.StatusTemporaryRedirect)
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
