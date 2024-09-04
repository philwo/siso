// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package webui implements siso webui.
package webui

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"text/template"
	"time"

	"infra/build/siso/build"
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

// ErrExecrootNotExist represents error when exec root was not found.
type ErrExecrootNotExist struct {
	err error
}

func (f ErrExecrootNotExist) Unwrap() error {
	return f.err
}

func (f ErrExecrootNotExist) Error() string {
	return fmt.Sprintf("failed to find execroot: %v", f.err)
}

// ErrManifestNotExist represents error when build manifest was not found.
type ErrManifestNotExist struct {
	outdirPath   string
	manifestPath string
}

func (f ErrManifestNotExist) Error() string {
	return fmt.Sprintf("%s not found in %s", f.manifestPath, f.outdirPath)
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

// baseURLFromContext gets the base URL from context.
// This is a HARDCODED assumption that siso webui only has routes that start with outdir.
func baseURLFromContext(ctx context.Context) (string, error) {
	outroot, ok := ctx.Value(OutrootContextKey).(string)
	if !ok {
		return "", fmt.Errorf("failed to get outroot from context")
	}
	outsub, ok := ctx.Value(OutsubContextKey).(string)
	if !ok {
		return "", fmt.Errorf("failed to get outsub from context")
	}
	return fmt.Sprintf("/%s/%s", outroot, outsub), nil
}

// renderBuildView renders a build-related view.
// TODO(b/361703735): return data instead of write to response writer? https://chromium-review.googlesource.com/c/infra/infra/+/5803123/comment/4ce69ada_31730349/
func (s *WebuiServer) renderBuildView(wr http.ResponseWriter, r *http.Request, tmpl *template.Template, outdirInfo *outdirInfo, data map[string]any) error {
	if currentBaseURL, err := baseURLFromContext(r.Context()); err == nil {
		data["currentBaseURL"] = currentBaseURL
	}
	// TODO(b/361461051): refactor rev so that it's not required as part of rendering pages in general.
	rev := r.PathValue("rev")
	if rev == "" {
		rev = "latest"
	}
	data["outsubs"] = s.outsubs
	data["versionID"] = s.sisoVersion
	data["currentURL"] = r.URL
	data["currentRev"] = rev
	// TODO(b/361461051): refactor so outdirInfo is not required as part of rendering pages in general.
	// the current outroot and outsub should be available from the context.
	if outdirInfo != nil {
		outdirBaseURL := fmt.Sprintf("/%s/%s", url.PathEscape(outdirInfo.outroot), url.PathEscape(outdirInfo.outsub))
		data["outdirBaseURL"] = outdirBaseURL
		data["outdirRevBaseURL"] = fmt.Sprintf("%s/builds/%s", outdirBaseURL, rev)
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
func NewServer(version string, localDevelopment bool, port int, defaultOutdir, configRepoDir, manifestPath string) (*WebuiServer, error) {
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
		return nil, &ErrExecrootNotExist{err}
	}

	// Preload default outdir.
	defaultOutdirInfo, err := loadOutdirInfo(s.execRoot, defaultOutdir, manifestPath)
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
	sseServer := newSseServer()
	sseServer.Start()
	http.Handle("/events/", sseServer)

	outdirRouter := s.outdirRouter(sseServer)

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
		baseURL := fmt.Sprintf("/%s/%s", outroot, outsub)
		ctx := r.Context()
		ctx = context.WithValue(ctx, OutrootContextKey, outroot)
		ctx = context.WithValue(ctx, OutsubContextKey, outsub)
		// Manually strip prefix.
		// (Can't strip patterns with net/http see https://github.com/golang/go/issues/64909)
		http.StripPrefix(baseURL, outdirRouter).ServeHTTP(w, r.WithContext(ctx))
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
