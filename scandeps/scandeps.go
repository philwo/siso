// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package scandeps

import (
	"context"
	"errors"
	"fmt"
	"hash/maphash"
	"strings"
	"time"

	"github.com/charmbracelet/log"
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
	started := time.Now()

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

	setupDur := time.Since(started)
	if setupDur > 500*time.Millisecond {
		log.Infof("scan setup dirs:%d %s", len(req.Dirs), setupDur)
	}
	started = time.Now()

	// max scandeps time in chromium/linux all build on P920 is 12s
	// as of 2023-06-26
	// but we see some timeout with 20s on linux-build-perf-developer builder
	// as of 2023-08-31 b/298142575, 2023-10-23 b/307202429
	// it was introduced to mitigate scanning that does not terminate,
	// but we see such scan recently, so set sufficient large timeout
	// to avoid scan failure due to timed out.
	scanTimeout := max(req.Timeout, 60*time.Second)

	icnt := 0
	ncnt := 0
	for scanner.hasInputs() {
		icnt++
		dur := time.Since(started)
		if dur > scanTimeout {
			return nil, fmt.Errorf("too slow scandeps: dirs:%d ds:%d i:%d n:%d %s %s", len(req.Dirs), scanner.maxDirstack, icnt, ncnt, setupDur, dur)
		}
		names := scanner.nextInputs(ctx)
		for _, name := range names {
			ncnt++
			incpath, err := scanner.find(ctx, name)
			if err != nil {
				if errors.Is(err, ctx.Err()) {
					return nil, fmt.Errorf("timeout dirs:%d ds:%d i:%d n:%d %s %s: %w", len(req.Dirs), scanner.maxDirstack, icnt, ncnt, setupDur, time.Since(started), err)
				}
				continue
			}
			if incpath == "" {
				// already read?
				continue
			}
			if deps, ok := s.inputDeps[incpath]; ok {
				scanner.addInputs(deps...)
			}
			// if not found, fallback to `clang -M`?
		}
	}
	results := scanner.results()
	return results, nil
}
