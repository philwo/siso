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
	"sync"
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
		"pathEscape": func(s string) string {
			return url.PathEscape(s)
		},
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
	sisoMetricsRe = regexp.MustCompile(`siso_metrics.(\d+).json`)
)

type outdirInfos struct {
	outdirs map[string]*outdirInfo
	mu      sync.Mutex
}

type outdirInfo struct {
	path    string
	metrics []*buildMetrics
}

type buildMetrics struct {
	mtime         time.Time
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

	stat, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("failed to stat metrics: %w", err)
	}

	metricsData := &buildMetrics{
		mtime:        stat.ModTime(),
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
	start := time.Now()
	fmt.Fprintf(os.Stderr, "load data at %s...", outdirPath)
	defer func() {
		fmt.Fprintf(os.Stderr, " returned in %v\n", time.Since(start))
	}()

	outdirInfo := &outdirInfo{
		path: outdirPath,
	}

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
		matches := sisoMetricsRe.FindStringSubmatch(baseName)
		if matches == nil {
			fmt.Fprintf(os.Stderr, "ignoring invalid %s\n", revPath)
			continue
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

// ensureOutdirForRequest lazy-loads outdir for the request, returning cached result if possible.
func ensureOutdirForRequest(r *http.Request, outdirInfos *outdirInfos, execRoot string) (*outdirInfo, error) {
	abs := filepath.Join(execRoot, r.PathValue("outroot"), r.PathValue("outsub"))
	outdirInfos.mu.Lock()
	defer outdirInfos.mu.Unlock()
	outdirInfo, ok := outdirInfos.outdirs[abs]
	if !ok {
		var err error
		outdirInfo, err = loadOutdirInfo(abs)
		if err != nil {
			return nil, fmt.Errorf("couldn't load outdir %s: %w", abs, err)
		}
		outdirInfos.outdirs[abs] = outdirInfo
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

	// Get execroot.
	execRoot, err := build.DetectExecRoot(defaultOutdir, configRepoDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to find execroot: %v", err)
		return 1
	}

	// Get path relative to execroot.
	// TODO: support paths non-relative to execroot?
	defaultOutdirExecRel, err := filepath.Rel(execRoot, defaultOutdir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "couldn't get outdir relative to execdir: %v", err)
		return 1
	}
	defaultOutdirParent, defaultOutdirBase := filepath.Split(defaultOutdirExecRel)
	if len(defaultOutdirParent) == 0 || strings.Contains(defaultOutdirBase, "/") {
		fmt.Fprintf(os.Stderr, "outdir must match pattern `execroot/outroot/outsub`, others are not supported yet")
		return 1
	}

	// Preload default outdir.
	outdirInfos := outdirInfos{
		outdirs: make(map[string]*outdirInfo),
	}
	defaultOutdirInfo, err := loadOutdirInfo(defaultOutdir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to preload outdir: %v", err)
		return 1
	}
	outdirInfos.outdirs[defaultOutdir] = defaultOutdirInfo

	// Find other outdirs.
	// TODO: support out*/*
	matches, err := filepath.Glob(filepath.Join(execRoot, "out/*"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to glob %s: %v", execRoot, err)
		return 1
	}
	var outsubs []string
	for _, match := range matches {
		m, err := os.Stat(match)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to stat outdir %s: %v", match, err)
			return 1
		}
		if m.IsDir() {
			outsub, err := filepath.Rel(filepath.Join(execRoot, defaultOutdirParent), match)
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to make %s execroot relative: %v", match, err)
				return 1
			}
			outsubs = append(outsubs, outsub)
		}
	}

	renderBuildView := func(wr http.ResponseWriter, r *http.Request, tmpl *template.Template, outdirInfo *outdirInfo, data map[string]any) error {
		outdirShort := outdirInfo.path
		if home, err := os.UserHomeDir(); err == nil {
			outdirShort = strings.Replace(outdirShort, home, "~", 1)
		}
		rev := r.PathValue("rev")
		data["prefix"] = fmt.Sprintf("/%s/%s/builds/%s", url.PathEscape(r.PathValue("outroot")), url.PathEscape(r.PathValue("outsub")), rev)
		data["outroot"] = r.PathValue("outroot")
		data["outsub"] = r.PathValue("outsub")
		data["outsubs"] = outsubs
		data["outdirShort"] = outdirShort
		data["versionID"] = version
		data["currentURL"] = r.URL
		data["currentRev"] = rev
		var knownRevs []string
		for _, m := range outdirInfo.metrics {
			if rev == m.rev {
				data["currentRevMtime"] = m.mtime.Format(time.RFC3339)
			}
			knownRevs = append(knownRevs, m.rev)
		}
		data["revs"] = knownRevs
		err := tmpl.ExecuteTemplate(wr, "base", data)
		if err != nil {
			return fmt.Errorf("failed to execute template: %w", err)
		}
		return nil
	}

	renderBuildViewError := func(status int, message string, w http.ResponseWriter, r *http.Request, outdirInfo *outdirInfo) {
		w.WriteHeader(status)
		tmpl, err := loadView(localDevelopment, fs, "_error.html")
		if err != nil {
			fmt.Fprintf(w, "failed to load error view: %s\n", err)
			return
		}
		err = renderBuildView(w, r, tmpl, outdirInfo, map[string]any{
			"errorTitle":   http.StatusText(status),
			"errorMessage": message,
		})
		if err != nil {
			fmt.Fprintf(w, "failed to render error view: %s\n", err)
		}
	}

	// Group together outdir related handlers.
	outdirHandlers := http.NewServeMux()

	outdirHandlers.HandleFunc("/{outroot}/{outsub}/reload", func(w http.ResponseWriter, r *http.Request) {
		// Don't try to reload non-existing outdir.
		outdirInfo, err := ensureOutdirForRequest(r, &outdirInfos, execRoot)
		if err != nil {
			renderBuildViewError(http.StatusNotFound, fmt.Sprintf("outdir failed to load for request %s: %v", r.URL, err), w, r, outdirInfo)
			return
		}

		// loadOutdirInfo will always override existing cached data.
		newOutdirInfo, err := loadOutdirInfo(outdirInfo.path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to reload outdir: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		outdirInfos.mu.Lock()
		outdirInfos.outdirs[outdirInfo.path] = newOutdirInfo
		outdirInfos.mu.Unlock()

		// Then redirect to steps page.
		dest := fmt.Sprintf(
			"/%s/%s/builds/latest/steps/",
			url.PathEscape(r.PathValue("outroot")),
			url.PathEscape(r.PathValue("outsub")))
		http.Redirect(w, r, dest, http.StatusTemporaryRedirect)
	})

	outdirHandlers.HandleFunc("/{outroot}/{outsub}/builds/{rev}/logs/", func(w http.ResponseWriter, r *http.Request) {
		dest := fmt.Sprintf(
			"/%s/%s/builds/%s/logs/.siso_config",
			url.PathEscape(r.PathValue("outroot")),
			url.PathEscape(r.PathValue("outsub")),
			url.PathEscape(r.PathValue("rev")))
		http.Redirect(w, r, dest, http.StatusTemporaryRedirect)
	})

	outdirHandlers.HandleFunc("/{outroot}/{outsub}/builds/{rev}/logs/{file}", func(w http.ResponseWriter, r *http.Request) {
		outdirInfo, err := ensureOutdirForRequest(r, &outdirInfos, execRoot)
		if err != nil {
			renderBuildViewError(http.StatusNotFound, fmt.Sprintf("outdir failed to load for request %s: %v", r.URL, err), w, r, outdirInfo)
			return
		}

		tmpl, err := loadView(localDevelopment, fs, "_logs.html")
		if err != nil {
			renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to load view: %s", err), w, r, outdirInfo)
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
			renderBuildViewError(http.StatusNotFound, fmt.Sprintf("unknown file: %s", requestedFile), w, r, outdirInfo)
			return
		}

		rev := r.PathValue("rev")
		actualFile := requestedFile
		if rev != "latest" {
			actualFile = fmt.Sprintf(revFileFormatter, rev)
		}
		fileContents, err := os.ReadFile(filepath.Join(defaultOutdir, actualFile))
		if err != nil {
			renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to open file: %v", err), w, r, outdirInfo)
			return
		}

		if r.URL.Query().Get("raw") == "true" {
			w.Header().Add("Content-Type", "text/plain; charset=UTF-8")
			_, err := w.Write(fileContents)
			if err != nil {
				renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to write file contents: %v", err), w, r, outdirInfo)
			}
			return
		}

		// TODO(b/349287453): use maps.Keys once go 1.23
		var allowedFiles []string
		for allowedFile := range allowedFilesMap {
			allowedFiles = append(allowedFiles, allowedFile)
		}
		slices.Sort(allowedFiles)

		err = renderBuildView(w, r, tmpl, outdirInfo, map[string]any{
			"allowedFiles": allowedFiles,
			"file":         requestedFile,
			"fileContents": string(fileContents),
		})
		if err != nil {
			renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to render view: %v", err), w, r, outdirInfo)
		}
	})

	outdirHandlers.HandleFunc("POST /{outroot}/{outsub}/builds/{rev}/steps/{id}/recall/", func(w http.ResponseWriter, r *http.Request) {
		outdirInfo, err := ensureOutdirForRequest(r, &outdirInfos, execRoot)
		if err != nil {
			renderBuildViewError(http.StatusNotFound, fmt.Sprintf("outdir failed to load for request %s: %v", r.URL, err), w, r, outdirInfo)
			return
		}
		var metrics *buildMetrics
		for _, m := range outdirInfo.metrics {
			if m.rev == r.PathValue("rev") {
				metrics = m
				break
			}
		}
		if metrics == nil {
			renderBuildViewError(http.StatusNotFound, fmt.Sprintf("no metrics found for request %s", r.URL), w, r, outdirInfo)
			return
		}

		tmpl, err := loadView(localDevelopment, fs, "_recall.html")
		if err != nil {
			renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to load view: %s", err), w, r, outdirInfo)
			return
		}

		metric, ok := metrics.stepMetrics[r.PathValue("id")]
		if !ok {
			renderBuildViewError(http.StatusNotFound, fmt.Sprintf("stepID %s not found", r.PathValue("id")), w, r, outdirInfo)
			return
		}

		err = renderBuildView(w, r, tmpl, outdirInfo, map[string]any{
			"stepID":        metric.StepID,
			"digest":        metric.Digest,
			"project":       r.FormValue("project"),
			"reapiInstance": r.FormValue("reapi_instance"),
		})
		if err != nil {
			renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to render view: %v", err), w, r, outdirInfo)
		}
	})

	outdirHandlers.HandleFunc("/{outroot}/{outsub}/builds/{rev}/steps/{id}/", func(w http.ResponseWriter, r *http.Request) {
		outdirInfo, err := ensureOutdirForRequest(r, &outdirInfos, execRoot)
		if err != nil {
			renderBuildViewError(http.StatusNotFound, fmt.Sprintf("outdir failed to load for request %s: %v", r.URL, err), w, r, outdirInfo)
			return
		}
		var metrics *buildMetrics
		for _, m := range outdirInfo.metrics {
			if m.rev == r.PathValue("rev") {
				metrics = m
				break
			}
		}
		if metrics == nil {
			renderBuildViewError(http.StatusNotFound, fmt.Sprintf("no metrics found for request %s", r.URL), w, r, outdirInfo)
			return
		}

		tmpl, err := loadView(localDevelopment, fs, "_step.html")
		if err != nil {
			renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to load view: %s", err), w, r, outdirInfo)
			return
		}

		metric, ok := metrics.stepMetrics[r.PathValue("id")]
		if !ok {
			renderBuildViewError(http.StatusNotFound, fmt.Sprintf("stepID %s not found", r.PathValue("id")), w, r, outdirInfo)
			return
		}

		var asMap map[string]any
		asJSON, err := json.Marshal(metric)
		if err != nil {
			renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to marshal metrics: %v", err), w, r, outdirInfo)
		}
		err = json.Unmarshal(asJSON, &asMap)
		if err != nil {
			renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to unmarshal metrics: %v", err), w, r, outdirInfo)
		}

		err = renderBuildView(w, r, tmpl, outdirInfo, asMap)
		if err != nil {
			renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to render view: %v", err), w, r, outdirInfo)
		}
	})

	outdirHandlers.HandleFunc("/{outroot}/{outsub}/builds/{rev}/steps/", func(w http.ResponseWriter, r *http.Request) {
		outdirInfo, err := ensureOutdirForRequest(r, &outdirInfos, execRoot)
		if err != nil {
			renderBuildViewError(http.StatusNotFound, fmt.Sprintf("outdir failed to load for request %s: %v", r.URL, err), w, r, outdirInfo)
			return
		}
		var metrics *buildMetrics
		for _, m := range outdirInfo.metrics {
			if m.rev == r.PathValue("rev") {
				metrics = m
				break
			}
		}
		if metrics == nil {
			renderBuildViewError(http.StatusNotFound, fmt.Sprintf("no metrics found for request %s", r.URL), w, r, outdirInfo)
			return
		}

		tmpl, err := loadView(localDevelopment, fs, "_steps.html")
		if err != nil {
			renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to load view: %s", err), w, r, outdirInfo)
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
		err = renderBuildView(w, r, tmpl, outdirInfo, data)
		if err != nil {
			renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to render view: %v", err), w, r, outdirInfo)
		}
	})

	// Default catch-all handler.
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Redirect root to default outdir.
		if r.URL.Path == "/" {
			http.Redirect(w, r, fmt.Sprintf("/%s/%s/builds/latest/steps/", defaultOutdirParent, defaultOutdirBase), http.StatusTemporaryRedirect)
			return
		}

		// Delegate all other requests to outdir handlers.
		// This is because outdir handlers start with a generic wildcard pattern "/{outroot}/...".
		// We should give outdir handlers fallback for all paths that aren't matched by any other handler.
		outdirHandlers.ServeHTTP(w, r)
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
