// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	log "github.com/golang/glog"

	"infra/build/siso/ui"
)

// experiment id -> hint for the experiment (to check more details).
var knownExperiments = map[string]string{
	"fail-on-stdouterr":           "",
	"file-access-trace":           "",
	"gvisor":                      "",
	"ignore-missing-local-inputs": "",
	"keep-going-handle-error":     "",
	"keep-going-impure":           "check siso_localexec",
	"no-fallback":                 "",
	"no-fast-deps-fallback":       "",
}

type experimentFeature struct {
	once sync.Once
}

// Experiments manages experimental features.
// Experiments are enabled by SISO_EXPERIMENTS environment variable.
// We don't guarantee experiment id in future versions.
// Unknown experiment id will be ignored.
type Experiments struct {
	once sync.Once
	m    map[string]*experimentFeature
}

const experimentEnv = "SISO_EXPERIMENTS"

func (e *Experiments) init() {
	if e.m != nil {
		return
	}
	env := os.Getenv(experimentEnv)
	if env == "" {
		return
	}
	e.m = make(map[string]*experimentFeature)
	for _, v := range strings.Split(env, ",") {
		if _, ok := knownExperiments[v]; !ok {
			log.Warningf("unknown experiment %q. ignored", v)
			continue
		}
		e.m[v] = &experimentFeature{}
	}
}

// ShowOnce shows once about enabled experimental features.
func (e *Experiments) ShowOnce() {
	e.init()
	e.once.Do(func() {
		s := e.String()
		if s != "" {
			s = ui.SGR(ui.Yellow, s)
			ui.Default.PrintLines(s)
		}
	})
}

func (e *Experiments) String() string {
	var sb strings.Builder
	keys := make([]string, 0, len(e.m))
	for key := range e.m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(&sb, "%s=%s enabled\n", experimentEnv, key)
	}
	return sb.String()
}

// Enabled returns true if experimental feature k is enabled, and
// log error once with its hint if so.
func (e *Experiments) Enabled(k, format string, args ...any) bool {
	ex, ok := e.m[k]
	if !ok {
		return false
	}
	ex.once.Do(func() {
		ui.Default.PrintLines(fmt.Sprintf(format+" %s\n", append(args, e.Hint(k))...))
	})
	return true
}

// Hint shows hint message for experimental feature k.
func (e *Experiments) Hint(k string) string {
	return knownExperiments[k]
}

// Suggest returns suggest message to enable experimental feature k.
func (e *Experiments) Suggest(k string) string {
	hint := knownExperiments[k]
	if hint != "" {
		return fmt.Sprintf("need %s=%s or %s", experimentEnv, k, hint)
	}
	return fmt.Sprintf("need %s=%s", experimentEnv, k)
}
