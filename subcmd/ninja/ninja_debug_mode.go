// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"fmt"
	"os"
	"strings"
)

// ninja compatible debug mode.
type debugMode struct {
	Stats       bool // print operation counts/timing info
	Explain     bool // explain what caused a command to execute
	Keepdepfile bool // don't delete depfiles after they're read by ninja
	Keeprsp     bool // don't delete @response files on success
	List        bool // lists modes
}

func (m *debugMode) check() bool {
	if m.List {
		fmt.Fprint(os.Stderr, `debugging modes
  stats        not implemented: print operation counts/timing info
  explain      not implemented b/288419130: explain what caused a command to execute
  keepdepfile  not implemented: don't delete depfiles after they're read by ninja
  keeprsp      don't delete @response files on success
multiple modes can be enabled via -d FOO -d BAR
`)
		return true
	}
	if m.Stats {
		fmt.Fprintln(os.Stderr, "WARNING: `-d stats` is not implemented yet")
	}
	if m.Explain {
		fmt.Fprintln(os.Stderr, "WARNING: `-d explain` is not implemented yet: b/288419130")
	}
	if m.Keepdepfile {
		fmt.Fprintln(os.Stderr, "WARNING: `-d keepdepfile` is not implemented yet")
	}
	return false

}

// String returns mode flag value as comma separated values.
func (m *debugMode) String() string {
	var modes []string
	if m.Stats {
		modes = append(modes, "stats")
	}
	if m.Explain {
		modes = append(modes, "explain")
	}
	if m.Keepdepfile {
		modes = append(modes, "keepdepfile")
	}
	if m.Keeprsp {
		modes = append(modes, "keeprsp")
	}
	return strings.Join(modes, ",")
}

// Set sets flag value for each flag present.
func (m *debugMode) Set(v string) error {
	for _, s := range strings.Split(v, ",") {
		switch s {
		case "stats":
			m.Stats = true
		case "explain":
			m.Explain = true
		case "keepdepfile":
			m.Keepdepfile = true
		case "keeprsp":
			m.Keeprsp = true
		case "list":
			m.List = true
		default:
			return fmt.Errorf("unknown debug setting %q", s)
		}
	}
	return nil
}
