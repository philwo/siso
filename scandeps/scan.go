// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package scandeps

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"path"
	"path/filepath"
	"strings"
	"sync"
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

	// macro name -> value -> used?
	macroUsed map[string]map[string]bool

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

	// hmap data: incpath -> filenames.
	hmaps map[string][]string

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

func (fsys *filesystem) scanner(ctx context.Context, execRoot string, inputDeps map[string][]string, precomputedTrees []string) *scanner {
	s := &scanner{
		fsview: &fsview{
			fs:               fsys,
			execRoot:         execRoot,
			inputDeps:        inputDeps,
			precomputedTrees: precomputedTrees,
			visited:          make(map[string]bool),
			dirs:             make(map[string]bool),
			files:            make(map[string]*scanResult),
			topEnts:          make(map[string]*sync.Map),
		},
		macros:       make(map[string][]string),
		included:     make(map[string]map[string]bool),
		macroUsed:    make(map[string]map[string]bool),
		macroInclude: make(map[string]bool),
		macroDirs:    make(map[string][]string),
		nameDirs:     make(map[string]int),
		hmaps:        make(map[string][]string),
	}
	for _, dir := range precomputedTrees {
		s.fsview.addDir(ctx, dir, noSearchPath)
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
			s.popDir()
			continue
		}
		s.names = s.names[:0]
		s.names = cppExpandMacros(ctx, s.names, incname, s.macros)
		return s.names
	}
	return nil
}

func (s *scanner) addInputs(ins ...string) {
	s.inputs = append(append([]string(nil), ins...), s.inputs...)
}

func (s *scanner) setMacros(macros map[string]string) {
	for k, v := range macros {
		s.macros[k] = append(s.macros[k], v)
	}
}

func (s *scanner) updateMacros(macros map[string][]string) {
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

func (s *scanner) addInclude(fname string) {
	// -include or /FI is equivalent with `#include "filename"`
	s.pushInputs(`"` + fname + `"`)
}

func (s *scanner) addSource(ctx context.Context, fname string) {
	// add dir and include as if #include "basename".
	s.pushDir(ctx, filepath.ToSlash(filepath.Dir(fname)))
	base := filepath.Base(fname)
	s.pushInputs(`"` + base + `"`)
}

func (s *scanner) addDir(ctx context.Context, dir string) {
	s.fsview.addDir(ctx, dir, includeSearchPath)
}

func (s *scanner) addFrameworkDir(ctx context.Context, dir string) {
	s.fsview.addDir(ctx, dir, frameworkSearchPath)
}

func (s *scanner) pushDir(ctx context.Context, dir string) {
	s.dirstack = append(s.dirstack, dir)
	if len(s.dirstack) > s.maxDirstack {
		s.maxDirstack = len(s.dirstack)
	}
	s.fsview.addDir(ctx, dir, noSearchPath)
}

func (s *scanner) popDir() {
	if len(s.dirstack) == 0 {
		return
	}
	s.dirstack = s.dirstack[:len(s.dirstack)-1]
}

func (s *scanner) addHmap(ctx context.Context, hmap string) bool {
	m, ok := s.fsview.getHmap(ctx, hmap)
	if !ok {
		return false
	}
	for k, v := range m {
		s.hmaps[k] = append(s.hmaps[k], v)
	}
	return true
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
	if filepath.IsAbs(name) {
		rel, err := filepath.Rel(s.fsview.execRoot, name)
		if err != nil || !filepath.IsLocal(rel) {
			return "", fmt.Errorf("unacceptable abs include path %q: %w", name, err)
		}
		if s.fsview.topEnts["."] == nil {
			s.fsview.addDir(ctx, ".", includeSearchPath)
		}
		incpath, sr, err := s.fsview.get(ctx, ".", rel)
		if err != nil {
			return "", err
		}
		s.macroCheck(".", rel, incpath, sr.includes)
		dir := path.Dir(incpath)
		s.pushDir(ctx, dir)
		s.updateMacros(sr.defines)
		s.pushInputs(sr.includes...)
		return incpath, nil
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
	if len(ds) > 0 {
		// TODO: lookup hmap appropriately.
		s.ds = ds
		for i, dir := range ds {
			if included[dir] {
				continue
			}
			included[dir] = true
			incpath, sr, err := s.fsview.get(ctx, dir, name)
			if err != nil {
				continue
			}
			s.macroCheck(dir, name, incpath, sr.includes)

			// `#include "xx"` in incpath may include "xx"
			// from the dir of incpath.
			dir := path.Dir(incpath)
			s.pushDir(ctx, dir)

			s.updateMacros(sr.defines)
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
	}
	if len(s.fsview.frameworkPaths) > 0 {
		fwdir, base, found := strings.Cut(name, "/")
		if found {
			// framework import "Foo/Bar.h" -> "Foo.framework/Headers/Bar.h"
			fwname := path.Join(fwdir+".framework", "Headers", base)
			for _, dir := range s.fsview.frameworkPaths {
				if included[dir] {
					continue
				}
				included[dir] = true
				incpath, sr, err := s.fsview.get(ctx, dir, fwname)
				if err != nil {
					continue
				}
				s.macroCheck(dir, name, incpath, sr.includes)

				// `#include "xx"` in incpath may include "xx"
				// from the dir of incpath.
				dir := path.Dir(incpath)
				s.pushDir(ctx, dir)

				s.updateMacros(sr.defines)
				s.pushInputs(sr.includes...)
				return incpath, nil
			}
		}
	}
	return "", fs.ErrNotExist
}

func (s *scanner) macroCheck(dir, name, incpath string, incnames []string) {
	for _, iname := range incnames {
		if isMacro(iname) && !s.macroAllUsed(iname) {
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

func (s *scanner) macroAllUsed(macro string) bool {
	if s.macroUsed[macro] == nil {
		s.macroUsed[macro] = make(map[string]bool)
	}
	values, ok := s.macros[macro]
	if !ok {
		return true
	}
	allUsed := true
	for _, v := range values {
		if !s.macroUsed[macro][v] {
			s.macroUsed[macro][v] = true
			allUsed = false
		}
	}
	return allUsed
}

func (s *scanner) results() []string {
	res := s.fsview.results()
	// add all files referred by hmap
	// TODO: add only used header in hmap.
	for _, v := range s.hmaps {
		res = append(res, v...)
	}
	return res
}
