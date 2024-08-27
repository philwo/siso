// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package webui implements siso webui.
package webui

import (
	"bufio"
	"bytes"
	"context"
	"embed"
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
