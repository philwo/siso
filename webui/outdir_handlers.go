// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package webui

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"infra/build/siso/build"
)

const (
	OutrootContextKey   = webuiContextKey("outroot")
	OutsubContextKey    = webuiContextKey("outsub")
	DefaultItemsPerPage = 100
)

type outdirInfo struct {
	path    string
	outroot string
	outsub  string
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

type aggregateMetric struct {
	// All fields must be exported to be usable in a Go template.
	AggregateBy           string
	Count                 int
	TotalUtime            build.IntervalMetric
	TotalDuration         build.IntervalMetric
	TotalWeightedDuration build.IntervalMetric
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
func loadOutdirInfo(execRoot, outdirPath string) (*outdirInfo, error) {
	start := time.Now()
	fmt.Fprintf(os.Stderr, "load data at %s...", outdirPath)
	defer func() {
		fmt.Fprintf(os.Stderr, " returned in %v\n", time.Since(start))
	}()

	// Get path relative to execroot.
	// TODO: support paths non-relative to execroot?
	defaultOutdirExecRel, err := filepath.Rel(execRoot, outdirPath)
	if err != nil {
		return nil, fmt.Errorf("couldn't get outdir relative to execdir: %w", err)
	}
	// TODO(b/361703735): make sure this works on windows? https://chromium-review.googlesource.com/c/infra/infra/+/5803123/comment/502308d3_ac05bf91/
	outroot, outsub := filepath.Split(defaultOutdirExecRel)
	if outroot == "" || strings.Contains(outsub, "/") {
		return nil, fmt.Errorf("outdir must match pattern `execroot/outroot/outsub`, others are not supported yet")
	}
	outroot = filepath.Clean(outroot)

	outdirInfo := &outdirInfo{
		path:    outdirPath,
		outroot: outroot,
		outsub:  outsub,
	}

	// Attempt to load latest metrics first.
	// Only silently ignore if it doesn't exist, otherwise always return error.
	// TODO(b/349287453): consider tolerate fail, so frontend can show error?
	latestMetricsPath := filepath.Join(outdirPath, "siso_metrics.json")
	_, err = os.Stat(latestMetricsPath)
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
func (s *WebuiServer) ensureOutdirForRequest(r *http.Request) (*outdirInfo, error) {
	abs := filepath.Join(s.execRoot, r.Context().Value(OutrootContextKey).(string), r.Context().Value(OutsubContextKey).(string))
	s.mu.Lock()
	defer s.mu.Unlock()
	outdirInfo, ok := s.outdirs[abs]
	if !ok {
		var err error
		outdirInfo, err = loadOutdirInfo(s.execRoot, abs)
		if err != nil {
			return nil, fmt.Errorf("couldn't load outdir %s: %w", abs, err)
		}
		s.outdirs[abs] = outdirInfo
	}
	return outdirInfo, nil
}

// outdirRouter returns *http.ServeMux with handlers related to an outdir.
// The consumer of this function is responsible for nesting this *http.ServeMux appropriately.
// All handlers assume request's context.Context contains outroot, outsub.
func (s *WebuiServer) outdirRouter(sseServer *sseServer) *http.ServeMux {
	outdirRouter := http.NewServeMux()

	outdirRouter.HandleFunc("/reload", func(w http.ResponseWriter, r *http.Request) {
		outdirInfo, err := s.ensureOutdirForRequest(r)
		if err != nil {
			s.renderBuildViewError(http.StatusNotFound, fmt.Sprintf("outdir failed to load for request %s: %v", r.URL, err), w, r, outdirInfo)
			return
		}

		// loadOutdirInfo will always override existing cached data.
		newOutdirInfo, err := loadOutdirInfo(s.execRoot, outdirInfo.path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to reload outdir: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		s.mu.Lock()
		s.outdirs[outdirInfo.path] = newOutdirInfo
		s.mu.Unlock()

		// Then redirect to steps page.
		if currentBaseURL, err := baseURLFromContext(r.Context()); err == nil {
			http.Redirect(w, r, fmt.Sprintf("%s/builds/latest/steps/", currentBaseURL), http.StatusTemporaryRedirect)
		} else {
			fmt.Fprintf(os.Stderr, "missing base url")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	})

	outdirRouter.Handle("/runbuild/", s.runBuildRouter(sseServer))

	outdirRouter.HandleFunc("/builds/{rev}/logs/", func(w http.ResponseWriter, r *http.Request) {
		if currentBaseURL, err := baseURLFromContext(r.Context()); err == nil {
			dest := fmt.Sprintf("%s/builds/%s/logs/.siso_config", currentBaseURL, url.PathEscape(r.PathValue("rev")))
			http.Redirect(w, r, dest, http.StatusTemporaryRedirect)
		} else {
			fmt.Fprintf(os.Stderr, "missing base url")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	})

	outdirRouter.HandleFunc("/builds/{rev}/logs/{file}", func(w http.ResponseWriter, r *http.Request) {
		outdirInfo, err := s.ensureOutdirForRequest(r)
		if err != nil {
			s.renderBuildViewError(http.StatusNotFound, fmt.Sprintf("outdir failed to load for request %s: %v", r.URL, err), w, r, outdirInfo)
			return
		}

		tmpl, err := s.loadView("_logs.html")
		if err != nil {
			s.renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to load view: %s", err), w, r, outdirInfo)
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
			s.renderBuildViewError(http.StatusNotFound, fmt.Sprintf("unknown file: %s", requestedFile), w, r, outdirInfo)
			return
		}

		rev := r.PathValue("rev")
		actualFile := requestedFile
		if rev != "latest" {
			actualFile = fmt.Sprintf(revFileFormatter, rev)
		}
		fileContents, err := os.ReadFile(filepath.Join(s.defaultOutdir, actualFile))
		if err != nil {
			s.renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to open file: %v", err), w, r, outdirInfo)
			return
		}

		if r.URL.Query().Get("raw") == "true" {
			w.Header().Add("Content-Type", "text/plain; charset=UTF-8")
			_, err := w.Write(fileContents)
			if err != nil {
				s.renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to write file contents: %v", err), w, r, outdirInfo)
			}
			return
		}

		// TODO(b/349287453): use maps.Keys once go 1.23
		var allowedFiles []string
		for allowedFile := range allowedFilesMap {
			allowedFiles = append(allowedFiles, allowedFile)
		}
		slices.Sort(allowedFiles)

		err = s.renderBuildView(w, r, tmpl, outdirInfo, map[string]any{
			"allowedFiles": allowedFiles,
			"file":         requestedFile,
			"fileContents": string(fileContents),
		})
		if err != nil {
			s.renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to render view: %v", err), w, r, outdirInfo)
		}
	})

	outdirRouter.HandleFunc("/builds/{rev}/aggregates/", func(w http.ResponseWriter, r *http.Request) {
		outdirInfo, err := s.ensureOutdirForRequest(r)
		if err != nil {
			s.renderBuildViewError(http.StatusNotFound, fmt.Sprintf("outdir failed to load for request %s: %v", r.URL, err), w, r, outdirInfo)
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
			s.renderBuildViewError(http.StatusNotFound, fmt.Sprintf("no metrics found for request %s", r.URL), w, r, outdirInfo)
			return
		}

		aggregates := make(map[string]aggregateMetric)
		for _, m := range metrics.stepMetrics {
			// Aggregate by rule if exists otherwise action.
			aggregateBy := m.Action
			if len(m.Rule) > 0 {
				aggregateBy = m.Rule
			}
			entry, ok := aggregates[aggregateBy]
			if !ok {
				entry.AggregateBy = aggregateBy
			}
			entry.Count++
			entry.TotalUtime += m.Utime
			entry.TotalDuration += m.Duration
			entry.TotalWeightedDuration += m.WeightedDuration
			aggregates[aggregateBy] = entry
		}

		// Sort by utime descending.
		// TODO(b/349287453): use maps.Values once go 1.23
		var sortedAggregates []aggregateMetric
		for _, v := range aggregates {
			sortedAggregates = append(sortedAggregates, v)
		}
		slices.SortFunc(sortedAggregates, func(a, b aggregateMetric) int {
			return cmp.Compare(b.TotalUtime, a.TotalUtime)
		})

		tmpl, err := s.loadView("_aggregates.html")
		if err != nil {
			s.renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to load view: %s", err), w, r, outdirInfo)
			return
		}

		err = s.renderBuildView(w, r, tmpl, outdirInfo, map[string]any{
			"aggregates": sortedAggregates,
		})
		if err != nil {
			s.renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to render view: %v", err), w, r, outdirInfo)
		}
	})

	outdirRouter.HandleFunc("POST /builds/{rev}/steps/{id}/recall/", func(w http.ResponseWriter, r *http.Request) {
		outdirInfo, err := s.ensureOutdirForRequest(r)
		if err != nil {
			s.renderBuildViewError(http.StatusNotFound, fmt.Sprintf("outdir failed to load for request %s: %v", r.URL, err), w, r, outdirInfo)
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
			s.renderBuildViewError(http.StatusNotFound, fmt.Sprintf("no metrics found for request %s", r.URL), w, r, outdirInfo)
			return
		}

		tmpl, err := s.loadView("_recall.html")
		if err != nil {
			s.renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to load view: %s", err), w, r, outdirInfo)
			return
		}

		metric, ok := metrics.stepMetrics[r.PathValue("id")]
		if !ok {
			s.renderBuildViewError(http.StatusNotFound, fmt.Sprintf("stepID %s not found", r.PathValue("id")), w, r, outdirInfo)
			return
		}

		err = s.renderBuildView(w, r, tmpl, outdirInfo, map[string]any{
			"stepID":        metric.StepID,
			"digest":        metric.Digest,
			"project":       r.FormValue("project"),
			"reapiInstance": r.FormValue("reapi_instance"),
		})
		if err != nil {
			s.renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to render view: %v", err), w, r, outdirInfo)
		}
	})

	outdirRouter.HandleFunc("/builds/{rev}/steps/{id}/", func(w http.ResponseWriter, r *http.Request) {
		outdirInfo, err := s.ensureOutdirForRequest(r)
		if err != nil {
			s.renderBuildViewError(http.StatusNotFound, fmt.Sprintf("outdir failed to load for request %s: %v", r.URL, err), w, r, outdirInfo)
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
			s.renderBuildViewError(http.StatusNotFound, fmt.Sprintf("no metrics found for request %s", r.URL), w, r, outdirInfo)
			return
		}

		tmpl, err := s.loadView("_step.html")
		if err != nil {
			s.renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to load view: %s", err), w, r, outdirInfo)
			return
		}

		metric, ok := metrics.stepMetrics[r.PathValue("id")]
		if !ok {
			s.renderBuildViewError(http.StatusNotFound, fmt.Sprintf("stepID %s not found", r.PathValue("id")), w, r, outdirInfo)
			return
		}

		var asMap map[string]any
		asJSON, err := json.Marshal(metric)
		if err != nil {
			s.renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to marshal metrics: %v", err), w, r, outdirInfo)
		}
		err = json.Unmarshal(asJSON, &asMap)
		if err != nil {
			s.renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to unmarshal metrics: %v", err), w, r, outdirInfo)
		}

		err = s.renderBuildView(w, r, tmpl, outdirInfo, map[string]any{
			"step": asMap,
		})
		if err != nil {
			s.renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to render view: %v", err), w, r, outdirInfo)
		}
	})

	outdirRouter.HandleFunc("/builds/{rev}/steps/", func(w http.ResponseWriter, r *http.Request) {
		outdirInfo, err := s.ensureOutdirForRequest(r)
		if err != nil {
			s.renderBuildViewError(http.StatusNotFound, fmt.Sprintf("outdir failed to load for request %s: %v", r.URL, err), w, r, outdirInfo)
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
			s.renderBuildViewError(http.StatusNotFound, fmt.Sprintf("no metrics found for request %s", r.URL), w, r, outdirInfo)
			return
		}

		tmpl, err := s.loadView("_steps.html")
		if err != nil {
			s.renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to load view: %s", err), w, r, outdirInfo)
			return
		}

		actionsWanted := r.URL.Query()["action"]
		rulesWanted := r.URL.Query()["rule"]
		view := r.URL.Query().Get("view")
		outputSearch := r.URL.Query().Get("q")

		sortBy := "ready"
		sortDescending := false
		sortSupported := true
		sortParam := r.URL.Query().Get("sort")
		if sortParamRe.MatchString(sortParam) {
			matches := sortParamRe.FindStringSubmatch(sortParam)
			sortBy = string(matches[sortParamRe.SubexpIndex("sortBy")])
			if string(matches[sortParamRe.SubexpIndex("order")]) == "Dsc" {
				sortDescending = true
			}
		} else if len(sortParam) > 0 {
			s.renderBuildViewError(http.StatusBadRequest, fmt.Sprintf("invalid sort param: %s", sortParam), w, r, outdirInfo)
			return
		}

		var filteredSteps []build.StepMetric
		switch view {
		case "criticalPath":
			sortSupported = false
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
				if view == "localOnly" && !m.IsLocal {
					continue
				}
				filteredSteps = append(filteredSteps, m)
			}
			switch sortBy {
			case "ready":
				slices.SortFunc(filteredSteps, func(a, b build.StepMetric) int {
					return cmp.Compare(a.Ready, b.Ready)
				})
			case "duration":
				slices.SortFunc(filteredSteps, func(a, b build.StepMetric) int {
					return cmp.Compare(a.Duration, b.Duration)
				})
			case "completion":
				slices.SortFunc(filteredSteps, func(a, b build.StepMetric) int {
					return cmp.Compare(a.Ready+a.Duration, b.Ready+b.Duration)
				})
			default:
				s.renderBuildViewError(http.StatusBadRequest, fmt.Sprintf("unknown sort column: %s", sortBy), w, r, outdirInfo)
				return
			}
			if sortDescending {
				slices.Reverse(filteredSteps)
			}
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
			"sortSupported":    sortSupported,
		}
		err = s.renderBuildView(w, r, tmpl, outdirInfo, data)
		if err != nil {
			s.renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to render view: %v", err), w, r, outdirInfo)
		}
	})

	return outdirRouter
}
