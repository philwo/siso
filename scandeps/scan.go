// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package scandeps

import (
	"context"
	"io"
	"io/fs"
	"path"
	"path/filepath"
	"sync"

	log "github.com/golang/glog"

	"infra/build/siso/o11y/clog"
)

// scanner is a C++ dependency scanner per request.
type scanner struct {
	fsview *fsview

	// dir stack for #include "...".
	dirstack    []string
	maxDirstack int

	// input stack
	inputs []string

	// macros for "xxx" or <xxx>.
	// macro may have various values
	// e.g.
	// #ifdef NDEBUG
	// # define MACRO X
	// #else
	// # define MACRO Y
	// #endif
	//  MACRO -> [X, Y]
	macros map[string][]string

	// name -> dir -> visited
	included map[string]map[string]bool

	// filename -> has #include MACRO
	macroInclude map[string]bool

	// incname -> dirs
	// include of incname should be processed again for dirs
	// as incname contains #include MACRO and MACRO may have been changed.
	macroDirs map[string][]string

	// incname -> index of dirs
	// include of incname was processed by the index of dirs,
	// so no need to process it again.
	nameDirs map[string]int

	// allocation
	ds    []string
	names []string
}

type scanResult struct {
	mu sync.Mutex

	done     bool
	includes []string
	defines  map[string][]string
	err      error
}

func (fsys *filesystem) scanner(ctx context.Context, execRoot string, inputDeps map[string][]string, sysroots []string) *scanner {
	s := &scanner{
		fsview: &fsview{
			fs:        fsys,
			execRoot:  execRoot,
			inputDeps: inputDeps,
			sysroots:  sysroots,
			visited:   make(map[string]bool),
			dirs:      make(map[string]bool),
			files:     make(map[string]*scanResult),
			topEnts:   make(map[string]*sync.Map),
		},
		macros:       make(map[string][]string),
		included:     make(map[string]map[string]bool),
		macroInclude: make(map[string]bool),
		macroDirs:    make(map[string][]string),
		nameDirs:     make(map[string]int),
	}
	for _, dir := range sysroots {
		s.fsview.addDir(ctx, dir, false)
	}
	return s
}

func (s *scanner) pushInputs(ins ...string) {
	s.inputs = append(s.inputs, "") // "" will trigger popDir
	for i := len(ins) - 1; i >= 0; i-- {
		s.inputs = append(s.inputs, ins[i])
	}
}

func (s *scanner) pushMacroInputs(ins ...string) {
	s.inputs = append(s.inputs, "") // pop dir
	// only include macro again. i.e. no need to include non-macro path.
	for i := len(ins) - 1; i >= 0; i-- {
		if isMacro(ins[i]) {
			s.inputs = append(s.inputs, ins[i])
		}
	}
}

func (s *scanner) hasInputs() bool {
	return len(s.inputs) > 0
}

func (s *scanner) popInput() string {
	in := s.inputs[len(s.inputs)-1]
	s.inputs = s.inputs[:len(s.inputs)-1]
	return in
}

func (s *scanner) nextInputs(ctx context.Context) []string {
	for s.hasInputs() {
		incname := s.popInput()
		if incname == "" {
			s.popDir(ctx)
			continue
		}
		s.names = s.names[:0]
		s.names = cppExpandMacros(ctx, s.names, incname, s.macros)
		return s.names
	}
	return nil
}

func (s *scanner) addInputs(ctx context.Context, ins ...string) {
	s.inputs = append(append([]string(nil), ins...), s.inputs...)
}

func (s *scanner) setMacros(ctx context.Context, macros map[string]string) {
	for k, v := range macros {
		s.macros[k] = append(s.macros[k], v)
	}
}

func (s *scanner) updateMacros(ctx context.Context, macros map[string][]string) {
	for k, vs := range macros {
		seen := make(map[string]bool)
		for _, v := range s.macros[k] {
			seen[v] = true
		}
		for _, v := range vs {
			if seen[v] {
				continue
			}
			seen[v] = true
			s.macros[k] = append(s.macros[k], v)
		}
	}
}

func (s *scanner) addSource(ctx context.Context, fname string) {
	// add dir and include as if #include "basename".
	s.pushDir(ctx, filepath.ToSlash(filepath.Dir(fname)))
	base := filepath.Base(fname)
	s.pushInputs(`"` + base + `"`)
}

func (s *scanner) addDir(ctx context.Context, dir string) {
	s.fsview.addDir(ctx, dir, true)
}

func (s *scanner) pushDir(ctx context.Context, dir string) {
	s.dirstack = append(s.dirstack, dir)
	if len(s.dirstack) > s.maxDirstack {
		s.maxDirstack = len(s.dirstack)
	}
	s.fsview.addDir(ctx, dir, false)
	if log.V(1) {
		clog.Infof(ctx, "push dir <- %s", dir)
	}
}

func (s *scanner) popDir(ctx context.Context) {
	dir := s.dirstack[len(s.dirstack)-1]
	s.dirstack = s.dirstack[:len(s.dirstack)-1]
	if log.V(1) {
		clog.Infof(ctx, "pop dir -> %s", dir)
	}
}

func (s *scanner) find(ctx context.Context, name string) (string, error) {
	if name == "" {
		return "", io.EOF
	}
	form := name[0] // '"' or '<'
	name = name[1 : len(name)-1]
	included, ok := s.included[name]
	if !ok {
		included = make(map[string]bool)
		s.included[name] = included
	}
	ds := s.ds[:0]
	if form == '"' {
		for i := len(s.dirstack) - 1; i >= 0; i-- {
			ds = append(ds, s.dirstack[i])
		}
	}
	qi := len(ds)
	ds = append(ds, s.macroDirs[name]...)
	mi := len(ds)
	ds = append(ds, s.fsview.searchPaths[s.nameDirs[name]:]...)
	if len(ds) == 0 {
		return "", nil
	}
	if log.V(1) {
		clog.Infof(ctx, "find %q dirs:%d", name, len(ds))
		clog.Infof(ctx, "dirs %d %d %q", qi, mi, ds)
	}
	s.ds = ds
	for i, dir := range ds {
		if included[dir] {
			continue
		}
		included[dir] = true
		if log.V(1) {
			clog.Infof(ctx, "find check %s/%s", dir, name)
		}
		incpath, sr, err := s.fsview.get(ctx, dir, name)
		if err != nil {
			continue
		}
		s.macroCheck(ctx, dir, name, incpath, sr.includes)

		// `#include "xx"` in incpath may include "xx"
		// from the dir of incpath.
		dir := path.Dir(incpath)
		s.pushDir(ctx, dir)

		if log.V(1) {
			clog.Infof(ctx, "find %s -> includes:%q defines:%q", incpath, sr.includes, sr.defines)
		}

		s.updateMacros(ctx, sr.defines)
		if i >= qi && i < mi {
			s.pushMacroInputs(sr.includes...)
		} else {
			s.pushInputs(sr.includes...)
		}
		if i > mi {
			s.nameDirs[name] += i - mi
		}
		return incpath, nil
	}
	s.nameDirs[name] = len(ds) - mi
	if log.V(1) {
		clog.Infof(ctx, "find %s %v", name, fs.ErrNotExist)
	}
	return "", fs.ErrNotExist
}

func (s *scanner) macroCheck(ctx context.Context, dir, name, incpath string, incnames []string) {
	for _, iname := range incnames {
		if isMacro(iname) {
			// incname uses macro.
			// need to try include again
			// because macro value may have been changed.
			if !s.macroInclude[incpath] {
				s.macroDirs[name] = append(s.macroDirs[name], dir)
			}
			s.macroInclude[incpath] = true
			s.included[name][dir] = false
			return
		}
	}
}

func (s *scanner) results() []string {
	return s.fsview.results()
}
