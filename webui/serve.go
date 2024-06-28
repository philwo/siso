// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package webui implements siso webui.
package webui

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"text/template"

	"infra/build/siso/build"
)

//go:embed *.html *.css
var content embed.FS

const DefaultItemsPerPage = 100

func Serve(localDevelopment bool, port int, metricsJSON string) int {
	// Prepare templates.
	// TODO(b/349287453): don't recompile templates every time if using embedded fs.
	fs := fs.FS(content)
	if localDevelopment {
		fs = os.DirFS("webui/")
	}

	// Read metrics.
	f, err := os.Open(metricsJSON)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to read metrics: %v", err)
		return 1
	}
	defer func() {
		err := f.Close()
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to close metrics: %v", err)
		}
	}()
	d := json.NewDecoder(f)
	var metrics []build.StepMetric
	for {
		var m build.StepMetric
		err := d.Decode(&m)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "parse error in %s:%d: %v", metricsJSON, d.InputOffset(), err)
			return 1
		}
		metrics = append(metrics, m)
	}

	actionCounts := make(map[string]int)
	for _, metric := range metrics {
		if len(metric.Action) > 0 {
			actionCounts[metric.Action]++
		}
	}

	ruleCounts := make(map[string]int)
	for _, metric := range metrics {
		if len(metric.Rule) > 0 {
			ruleCounts[metric.Rule]++
		}
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

		stepIdx := slices.IndexFunc(metrics, func(s build.StepMetric) bool {
			return s.StepID == r.PathValue("id")
		})
		if stepIdx == -1 {
			http.NotFound(w, r)
			return
		}

		err = tmpl.ExecuteTemplate(w, "base", map[string]string{
			"step_id":        metrics[stepIdx].StepID,
			"digest":         metrics[stepIdx].Digest,
			"project":        r.FormValue("project"),
			"reapi_instance": r.FormValue("reapi_instance"),
		})
		if err != nil {
			fmt.Fprintf(w, "failed to execute template: %s\n", err)
		}
	})

	http.HandleFunc("/steps/{id}/", func(w http.ResponseWriter, r *http.Request) {
		templates := []string{"base.html", "_step.html"}
		tmpl, err := template.ParseFS(fs, templates...)
		if err != nil {
			fmt.Fprintf(w, "failed to parse templates: %s\n", err)
			return
		}

		stepIdx := slices.IndexFunc(metrics, func(s build.StepMetric) bool {
			return s.StepID == r.PathValue("id")
		})
		if stepIdx == -1 {
			http.NotFound(w, r)
			return
		}

		var asMap map[string]any
		asJSON, err := json.Marshal(metrics[stepIdx])
		if err != nil {
			// TODO(b/349287453): proper error handling.
			fmt.Fprintf(w, "failed to marshal metrics: %v\n", err)
		}
		err = json.Unmarshal(asJSON, &asMap)
		if err != nil {
			// TODO(b/349287453): proper error handling.
			fmt.Fprintf(w, "failed to unmarshal metrics: %v\n", err)
		}

		err = tmpl.ExecuteTemplate(w, "base", asMap)
		if err != nil {
			fmt.Fprintf(w, "failed to execute template: %v\n", err)
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
		}).ParseFS(fs, templates...)
		if err != nil {
			fmt.Fprintf(w, "failed to parse templates: %s\n", err)
			return
		}

		actionsWanted := r.URL.Query()["action"]
		rulesWanted := r.URL.Query()["rule"]
		outputSearch := r.URL.Query().Get("q")

		filteredMetrics := metrics
		if len(actionsWanted) > 0 || len(rulesWanted) > 0 || len(outputSearch) > 0 {
			// Need to clone metrics otherwise deletes will propagate to the cached metrics.
			filteredMetrics = slices.Clone(metrics)
			filteredMetrics = slices.DeleteFunc(filteredMetrics, func(m build.StepMetric) bool {
				shouldDelete := false
				// Perform filtering only if filters are set for that field.
				if len(actionsWanted) > 0 {
					if !slices.Contains(actionsWanted, m.Action) {
						shouldDelete = true
					}
				}
				if len(rulesWanted) > 0 {
					if !slices.Contains(rulesWanted, m.Rule) {
						shouldDelete = true
					}
				}
				if len(outputSearch) > 0 {
					if !strings.Contains(m.Output, outputSearch) {
						shouldDelete = true
					}
				}
				return shouldDelete
			})
		}

		itemsLen := len(filteredMetrics)
		itemsPerPage, err := strconv.Atoi(r.URL.Query().Get("items_per_page"))
		if err != nil {
			itemsPerPage = DefaultItemsPerPage
		}
		requestedPage, err := strconv.Atoi(r.URL.Query().Get("page"))
		if err != nil {
			requestedPage = 0
		}
		pageCount := len(filteredMetrics) / itemsPerPage
		if len(filteredMetrics)%itemsPerPage > 0 {
			pageCount++
		}

		pageFirst := 0
		pageLast := pageCount - 1
		pageIndex := max(0, min(requestedPage, pageLast))
		pageNext := min(pageIndex+1, pageLast)
		pagePrev := max(0, pageIndex-1)
		itemsFirst := pageIndex * itemsPerPage
		itemsLast := max(0, min(itemsFirst+itemsPerPage, itemsLen)-1)
		subset := filteredMetrics[itemsFirst:itemsLast]

		data := map[string]any{
			"subset":        subset,
			"output_search": outputSearch,
			"page":          requestedPage,
			"page_index":    pageIndex,
			"page_first":    pageFirst,
			"page_next":     pageNext,
			"page_prev":     pagePrev,
			"page_last":     pageLast,
			"page_count":    pageCount,
			"items_first":   itemsFirst + 1,
			"items_last":    itemsLast + 1,
			"items_len":     len(filteredMetrics),
			"action_counts": actionCounts,
			"rule_counts":   ruleCounts,
		}
		err = tmpl.ExecuteTemplate(w, "base", data)
		if err != nil {
			fmt.Fprintf(w, "failed to execute template: %s\n", err)
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
