// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package scandeps provides forged C/C++ dependency scanner.
// Compared with Goma's input processor, it only supports simple
// form of C preprocessor directives and uses precomputed subtree
// for sysroots or complicated include dirs.
//
// It only checks the following forms of #include
//
//	#include "foo.h"
//	#include <foo.h>
//	#include FOO_H
//
// to support last case, it also checks the following forms of #define
//
//	#define FOO_H "foo.h"
//	#define FOO_H <foo.h>
//	#define FOO_H OTHER_FOO_H
//
// Since it doesn't process `#if` or `#ifdef`, it expands all possible
// values of macros for `#include FOO_H`.  Using extra inputs is
// not problem, but may have potential cache miss issues, since
// there is discrepancy between simple scandeps vs clang's *.d outputs.
// TODO(b/283341125): fix cache miss issue.
//
// It doesn't allow comments nor multiline (\ at the end of line)
// for the directives.
//
// Also it uses input_deps's label for sysroots etc.
// if include dir or sysroot dir has label with `:headers`,
// it adds files of the input_deps instead of scanning files
// in the dir.  Rather using minimum sets of include dirs,
// it may use more files, but can use precomputed merkletree
// to improve performance in digest calculation for action inputs.
package scandeps

import (
	"context"
	"hash/maphash"
	"strings"
	"time"

	"go.chromium.org/infra/build/siso/hashfs"
)

// ScanDeps is a simple C/C++ dependency scanner.
type ScanDeps struct {
	fs *filesystem

	inputDeps map[string][]string
}

// New creates new ScanDeps.
func New(hashfs *hashfs.HashFS, inputDeps map[string][]string) *ScanDeps {
	s := &ScanDeps{
		fs: &filesystem{
			hashfs: hashfs,
			seed:   maphash.MakeSeed(),
		},
		inputDeps: inputDeps,
	}
	hashfs.Notify(s.fs.update)
	return s
}

// Request is a request to scan deps.
type Request struct {
	// Defines are defined macros (on command line).
	// macro value would be `"path.h"` or `<path.h>`
	Defines map[string]string

	// Sources are source files.
	Sources []string

	// Includes are additional include files (i.e. -include or /FI).
	// it would be equivalent with `#include "fname"` in source.
	Includes []string

	// Dirs are include directories (search paths) or hmap paths.
	Dirs []string

	// Frameworks are framework directories (search paths).
	Frameworks []string

	// Sysroots are sysroot directories.
	// It also includes toolchain root directory.
	Sysroots []string

	// To mitigate scanning that does not terminate.
	Timeout time.Duration
}

// Scan scans C/C++ source/header files for req to get C/C++ dependencies.
func (s *ScanDeps) Scan(ctx context.Context, execRoot string, req Request) ([]string, error) {
	// Assume sysroots use precomputed tree.
	var precomputedTrees []string
	precomputedTrees = append(precomputedTrees, req.Sysroots...)
	// framework, or some system include dirs may also use precomputed tree
	// if precomputed tree is defined for the dir (in addDir later).

	scanner := s.fs.scanner(ctx, execRoot, s.inputDeps, precomputedTrees)
	scanner.setMacros(req.Defines)

	for _, s := range req.Includes {
		scanner.addInclude(s)
	}
	for _, s := range req.Sources {
		scanner.addSource(ctx, s)
	}
	for _, dir := range req.Dirs {
		if strings.HasSuffix(dir, ".hmap") && scanner.addHmap(ctx, dir) {
			continue
		}
		scanner.addDir(ctx, dir)
	}
	for _, dir := range req.Frameworks {
		scanner.addFrameworkDir(ctx, dir)
	}

	icnt := 0
	ncnt := 0
	for scanner.hasInputs() {
		icnt++
		names := scanner.nextInputs(ctx)
		for _, name := range names {
			ncnt++
			incpath, err := scanner.find(ctx, name)
			if err != nil {
				continue
			}
			if incpath == "" {
				// already read?
				continue
			}
			if deps, ok := s.inputDeps[incpath]; ok {
				scanner.addInputs(deps...)
			}
		}
	}
	results := scanner.results()
	return results, nil
}
