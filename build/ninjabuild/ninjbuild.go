// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package ninjabuild provides build steps by ninja.
package ninjabuild

import (
	"sync"

	"infra/build/siso/build"
	"infra/build/siso/build/buildconfig"
	"infra/build/siso/hashfs"
	"infra/build/siso/toolsupport/ninjautil"
)

// Graph holds build graph, i.e. all step defs described in build.ninja.
type Graph struct {
	fname   string
	once    sync.Once
	initErr error
	nstate  *ninjautil.State

	visited map[*ninjautil.Edge]bool

	globals *globals
}

type globals struct {
	path    *build.Path
	hashFS  *hashfs.HashFS
	depsLog *ninjautil.DepsLog

	buildConfig    *buildconfig.Config
	stepConfig     *StepConfig
	replaces       map[string][]string
	accumulates    map[string][]string
	caseSensitives map[string][]string
	phony          map[string]bool
}
