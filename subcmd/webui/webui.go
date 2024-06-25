// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package webui provides webui subcommand.
package webui

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/maruel/subcommands"
)

const DefaultItemsPerPage = 100

func Cmd() *subcommands.Command {
	return &subcommands.Command{
		UsageLine: "webui <args>",
		Advanced:  true,
		ShortDesc: "starts the experimental webui",
		LongDesc:  "Starts the experimental webui. Not ready for wide use yet, requires static files to work. This is subject to breaking changes at any moment.",
		CommandRun: func() subcommands.CommandRun {
			r := &webuiRun{}
			r.init()
			return r
		},
	}
}

type webuiRun struct {
	subcommands.CommandRunBase
	port        int
	metricsJson string
}

func (c *webuiRun) init() {
	c.Flags.IntVar(&c.port, "port", 8080, "port to use (defaults to 8080)")
	c.Flags.StringVar(&c.metricsJson, "metrics", "", "path to siso_metrics.json")
}

func (c *webuiRun) Run(a subcommands.Application, args []string, env subcommands.Env) int {
	// Read metrics.
	b, err := os.ReadFile(c.metricsJson)
	if err != nil {
		fmt.Printf("failed to read metrics: %s\n", err)
		return 1
	}
	b = bytes.Replace(b, []byte("\n"), []byte(","), -1)
	b = append([]byte{'['}, b...)
	if b[len(b)-1] == ',' {
		b = b[:len(b)-1]
	}
	b = append(b, ']')
	var metrics []any
	err = json.Unmarshal(b, &metrics)
	if err != nil {
		fmt.Printf("failed to unmarshal metrics: %s\n", err)
		return 1
	}

	actionCounts := make(map[string]int)
	for _, metric := range metrics {
		if actionVal, ok := metric.(map[string]any)["action"]; ok {
			if action, ok := actionVal.(string); ok && len(action) > 0 {
				actionCounts[action]++
			}
		}
	}

	ruleCounts := make(map[string]int)
	for _, metric := range metrics {
		if ruleVal, ok := metric.(map[string]any)["rule"]; ok {
			if rule, ok := ruleVal.(string); ok && len(rule) > 0 {
				ruleCounts[rule]++
			}
		}
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/steps/", http.StatusTemporaryRedirect)
	})

	http.HandleFunc("/steps/{id}/", func(w http.ResponseWriter, r *http.Request) {
		tmpl, err := template.ParseFiles(
			"webui/base.html",
			"webui/_step.html",
		)
		if err != nil {
			fmt.Fprintf(w, "failed to parse templates: %s\n", err)
			return
		}

		// TODO: send 404 if missing
		step := slices.IndexFunc(metrics, func(m any) bool {
			if m, _ := m.(map[string]any); m != nil {
				if s, _ := m["step_id"].(string); s != "" {
					return s == r.PathValue("id")
				}
			}
			return false
		})

		err = tmpl.ExecuteTemplate(w, "base", metrics[step])
		if err != nil {
			fmt.Fprintf(w, "failed to execute template: %s\n", err)
		}
	})

	http.HandleFunc("/steps/", func(w http.ResponseWriter, r *http.Request) {
		tmpl, err := template.New("_steps.html").Funcs(template.FuncMap{
			"currentURLHasParam": func(key string, value string) bool {
				q := r.URL.Query()
				if values, ok := q[key]; ok {
					return slices.Contains(values, value)
				}
				return false
			},
		}).ParseFiles(
			"webui/base.html",
			"webui/_steps.html",
		)
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
			filteredMetrics = make([]any, len(metrics))
			copy(filteredMetrics, metrics)
			filteredMetrics = slices.DeleteFunc(filteredMetrics, func(m any) bool {
				shouldDelete := false
				if metric, ok := m.(map[string]any); ok {
					// Perform filtering only if filters are set for that field.
					// Ignore failed type assertions because null fields should be filtered out.
					if len(actionsWanted) > 0 {
						action, _ := metric["action"].(string)
						if !slices.Contains(actionsWanted, action) {
							shouldDelete = true
						}
					}
					if len(rulesWanted) > 0 {
						rule, _ := metric["rule"].(string)
						if !slices.Contains(rulesWanted, rule) {
							shouldDelete = true
						}
					}
					if len(outputSearch) > 0 {
						output, _ := metric["output"].(string)
						if !strings.Contains(output, outputSearch) {
							shouldDelete = true
						}
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
		http.ServeFile(w, r, "webui/style.css")
	})

	fmt.Printf("listening on http://localhost:%d/...\n", c.port)
	err = http.ListenAndServe(fmt.Sprintf(":%d", c.port), nil)
	if errors.Is(err, http.ErrServerClosed) {
		fmt.Printf("server closed\n")
	} else if err != nil {
		fmt.Printf("error starting server: %s\n", err)
		return 1
	}
	return 0
}
