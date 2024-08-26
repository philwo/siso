// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package webui implements siso webui.
package webui

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"

	"infra/build/siso/build"
	"infra/build/siso/ui"
	mwc "infra/third_party/material_web_components"
)

//go:embed *.html css/*.css
var content embed.FS

type webuiContextKey string

const (
	OutrootContextKey   = webuiContextKey("outroot")
	OutsubContextKey    = webuiContextKey("outsub")
	DefaultItemsPerPage = 100
)

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
		"urlParamGet": func(url *url.URL, key string) string {
			return url.Query().Get(key)
		},
		"urlParamEq": func(url *url.URL, key, value string) bool {
			return url.Query().Get(key) == value
		},
		"urlHasParam": func(url *url.URL, key, value string) bool {
			return slices.Contains(url.Query()[key], value)
		},
		"urlParamIsSet": func(url *url.URL, key string) bool {
			return len(url.Query()[key]) > 0
		},
		"urlParamReplace": func(url *url.URL, key, value string) *url.URL {
			query := url.Query()
			query.Set(key, value)
			url.RawQuery = query.Encode()
			return url
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
	ninjaStepRe   = regexp.MustCompile(`\[(?P<stepNum>[0-9]+?)/(?P<totalSteps>[0-9]+?)\] (?P<time>[^\s]+?) (?P<status>[SF]) (?P<type>[^\s]+?) (?P<out>.+)`)
	sortParamRe   = regexp.MustCompile(`^(?P<sortBy>[a-z]+?)(?P<order>Asc|Dsc)$`)
)

type WebuiServer struct {
	sisoVersion       string
	localDevelopment  bool
	port              int
	templatesFS       fs.FS
	execRoot          string
	defaultOutdir     string
	defaultOutdirRoot string
	defaultOutdirSub  string
	outsubs           []string

	mu      sync.Mutex
	outdirs map[string]*outdirInfo
}

type runningStepInfo struct {
	stepOut  string
	stepType string
	started  time.Time
}

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

// loadView lazy-parses a view once, or parses every time if in local development mode.
func (s *WebuiServer) loadView(view string) (*template.Template, error) {
	if template, ok := templates[view]; ok {
		return template, nil
	}
	template, err := template.New("").Funcs(baseFunctions).ParseFS(s.templatesFS, "base.html", view)
	if err != nil {
		return nil, fmt.Errorf("failed to parse view: %w", err)
	}
	if !s.localDevelopment {
		templates[view] = template
	}
	return template, nil
}

// renderBuildView renders a build-related view.
// TODO(b/361703735): return data instead of write to response writer? https://chromium-review.googlesource.com/c/infra/infra/+/5803123/comment/4ce69ada_31730349/
func (s *WebuiServer) renderBuildView(wr http.ResponseWriter, r *http.Request, tmpl *template.Template, outdirInfo *outdirInfo, data map[string]any) error {
	rev := r.PathValue("rev")
	data["outsubs"] = s.outsubs
	data["versionID"] = s.sisoVersion
	data["currentURL"] = r.URL
	data["currentRev"] = rev
	if outdirInfo != nil {
		data["prefix"] = fmt.Sprintf("/%s/%s/builds/%s", url.PathEscape(outdirInfo.outroot), url.PathEscape(outdirInfo.outsub), rev)
		data["outroot"] = outdirInfo.outroot
		data["outsub"] = outdirInfo.outsub
		outdirShort := outdirInfo.path
		// Showing the full path is too long in the webui so abbreviate home dir to ~.
		// TODO(b/361703735): refactor https://chromium-review.googlesource.com/c/infra/infra/+/5804478/comment/dcfb372d_f21e4cc5/
		if home, err := os.UserHomeDir(); err == nil {
			outdirShort = strings.Replace(outdirShort, home, "~", 1)
		}
		data["outdirShort"] = outdirShort
		var knownRevs []string
		for _, m := range outdirInfo.metrics {
			if rev == m.rev {
				data["currentRevMtime"] = m.mtime.Format(time.RFC3339)
			}
			knownRevs = append(knownRevs, m.rev)
		}
		data["revs"] = knownRevs
	}
	err := tmpl.ExecuteTemplate(wr, "base", data)
	if err != nil {
		return fmt.Errorf("failed to execute template: %w", err)
	}
	return nil
}

// renderBuildViewError renders a build-related error.
func (s *WebuiServer) renderBuildViewError(status int, message string, w http.ResponseWriter, r *http.Request, outdirInfo *outdirInfo) {
	w.WriteHeader(status)
	tmpl, err := s.loadView("_error.html")
	if err != nil {
		fmt.Fprintf(w, "failed to load error view: %s\n", err)
		return
	}
	err = s.renderBuildView(w, r, tmpl, outdirInfo, map[string]any{
		"errorTitle":   http.StatusText(status),
		"errorMessage": message,
	})
	if err != nil {
		fmt.Fprintf(w, "failed to render error view: %s\n", err)
	}
}

// NewServer inits a webui server.
func NewServer(version string, localDevelopment bool, port int, defaultOutdir, configRepoDir string) (*WebuiServer, error) {
	s := WebuiServer{
		sisoVersion:      version,
		localDevelopment: localDevelopment,
		templatesFS:      fs.FS(content),
		defaultOutdir:    defaultOutdir,
		outdirs:          make(map[string]*outdirInfo),
		port:             port,
	}
	if localDevelopment {
		s.templatesFS = os.DirFS("webui/")
	}

	// Get execroot.
	var err error
	s.execRoot, err = build.DetectExecRoot(defaultOutdir, configRepoDir)
	if err != nil {
		return nil, fmt.Errorf("failed to find execroot: %w", err)
	}

	// Preload default outdir.
	defaultOutdirInfo, err := loadOutdirInfo(s.execRoot, defaultOutdir)
	if err != nil {
		return nil, fmt.Errorf("failed to preload outdir: %w", err)
	}
	s.outdirs[defaultOutdir] = defaultOutdirInfo

	// Find other outdirs.
	// TODO: support out*/*
	// TODO(b/361703735): can use defaultOutdirParent?
	matches, err := filepath.Glob(filepath.Join(s.execRoot, "out/*"))
	if err != nil {
		return nil, fmt.Errorf("failed to glob %s: %w", s.execRoot, err)
	}
	for _, match := range matches {
		m, err := os.Stat(match)
		if err != nil {
			return nil, fmt.Errorf("failed to stat outdir %s: %w", match, err)
		}
		if m.IsDir() {
			outsub, err := filepath.Rel(filepath.Join(s.execRoot, defaultOutdirInfo.outroot), match)
			if err != nil {
				return nil, fmt.Errorf("failed to make %s execroot relative: %w", match, err)
			}
			s.outsubs = append(s.outsubs, outsub)
		}
	}

	return &s, nil
}

func (s *WebuiServer) Serve() int {
	sse := newSseServer()
	sse.Start()
	http.Handle("/events/", sse)

	outdirRouter := s.outdirRouter()

	// TODO: elevate this url to /builds/run/
	outdirRouter.HandleFunc("GET /builds/{rev}/run/", func(w http.ResponseWriter, r *http.Request) {
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

	outdirRouter.HandleFunc("POST /builds/{rev}/run/", func(w http.ResponseWriter, r *http.Request) {
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

					sse.messages <- sseMessage{"buildstatus", fmt.Sprintf("<li>%d active steps<li>%d/%s steps done", len(activeSteps), maxStep, totalSteps)}
				} else {
					sse.messages <- sseMessage{"buildlog", fmt.Sprintf("<div>%s</div>", line)}
				}
				line, err = reader.ReadString('\n')
			}
			fmt.Fprintf(os.Stderr, "A build was finished\n")
		}(pipe)
		go func() {
			for {
				select {
				case <-done:
					sse.messages <- sseMessage{"activesteps", "Finished"}
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
					sse.messages <- sseMessage{"activesteps", b.String()}
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

	// Handler for all outdir related URLs that loads the outdir and delegates to the outdir sub-router.
	// This is set up on a separate mux because it's too generic and would otherwise cause panic:
	//     /css/ and /{outroot}/{outsub}/ both match some paths, like "/css/outsub/".
	//     But neither is more specific than the other.
	//     /css/ matches "/css/", but /{outroot}/{outsub}/ doesn't.
	//     /{outroot}/{outsub}/ matches "/outroot/outsub/", but /css/ doesn't.
	outdirParser := http.NewServeMux()
	outdirParser.HandleFunc("/{outroot}/{outsub}/", func(w http.ResponseWriter, r *http.Request) {
		outroot := r.PathValue("outroot")
		outsub := r.PathValue("outsub")
		ctx := r.Context()
		ctx = context.WithValue(ctx, OutrootContextKey, outroot)
		ctx = context.WithValue(ctx, OutsubContextKey, outsub)
		// Manually strip prefix.
		// (Can't strip patterns with net/http see https://github.com/golang/go/issues/64909)
		http.StripPrefix(fmt.Sprintf("/%s/%s", outroot, outsub), outdirRouter).ServeHTTP(w, r.WithContext(ctx))
	})

	// Default catch-all handler.
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Redirect root to default outdir.
		if r.URL.Path == "/" {
			http.Redirect(w, r, fmt.Sprintf("/%s/%s/builds/latest/steps/", s.defaultOutdirRoot, s.defaultOutdirSub), http.StatusTemporaryRedirect)
			return
		}

		// Delegate all other requests to the outdir handler.
		outdirParser.ServeHTTP(w, r)
	})

	http.Handle("/css/", http.FileServerFS(s.templatesFS))

	// Serve third party JS. No other third party libraries right now, so just serve Material Design node_modules root.
	http.Handle("/third_party/", http.StripPrefix("/third_party/", http.FileServerFS(mwc.NodeModulesFS)))

	fmt.Printf("listening on http://localhost:%d/...\n", s.port)
	err := http.ListenAndServe(fmt.Sprintf(":%d", s.port), nil)
	if errors.Is(err, http.ErrServerClosed) {
		fmt.Printf("server closed\n")
	} else if err != nil {
		fmt.Printf("error starting server: %s\n", err)
		return 1
	}
	return 0
}

// outdirRouter returns *http.ServeMux with handlers related to an outdir.
// All handlers assume request's context.Context contains outdirInfo.
// TODO(b/361703735): Move this function into a separate file e.g. outdir_handlers.go.
func (s *WebuiServer) outdirRouter() *http.ServeMux {
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
		dest := fmt.Sprintf(
			"/%s/%s/builds/latest/steps/",
			url.PathEscape(r.PathValue("outroot")),
			url.PathEscape(r.PathValue("outsub")))
		http.Redirect(w, r, dest, http.StatusTemporaryRedirect)
	})

	outdirRouter.HandleFunc("/builds/{rev}/logs/", func(w http.ResponseWriter, r *http.Request) {
		dest := fmt.Sprintf(
			"/%s/%s/builds/%s/logs/.siso_config",
			url.PathEscape(r.PathValue("outroot")),
			url.PathEscape(r.PathValue("outsub")),
			url.PathEscape(r.PathValue("rev")))
		http.Redirect(w, r, dest, http.StatusTemporaryRedirect)
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
