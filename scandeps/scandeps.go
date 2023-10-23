// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package scandeps

import (
	"context"
	"errors"
	"fmt"
	"hash/maphash"
	"time"

	log "github.com/golang/glog"

	"infra/build/siso/hashfs"
	"infra/build/siso/o11y/clog"
	"infra/build/siso/o11y/trace"
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
	Includes []string

	// Dirs are include directory (search paths).
	Dirs []string

	// TODO: Frameworks []string

	// Sysroots are sysroot directory.
	// It also includes toolchain root directory.
	Sysroots []string
}

// Scan scans C/C++ source/header files for req to get C/C++ dependencies.
func (s *ScanDeps) Scan(ctx context.Context, execRoot string, req Request) ([]string, error) {
	ctx, span := trace.NewSpan(ctx, "scandeps")
	defer span.Close(nil)

	started := time.Now()

	scanner := s.fs.scanner(ctx, execRoot, s.inputDeps, req.Sysroots)
	scanner.setMacros(ctx, req.Defines)

	for _, s := range req.Includes {
		scanner.addInclude(ctx, s)
	}
	for _, s := range req.Sources {
		scanner.addSource(ctx, s)
	}
	for _, dir := range req.Dirs {
		scanner.addDir(ctx, dir)
	}

	setupDur := time.Since(started)
	if setupDur > 500*time.Millisecond {
		clog.Infof(ctx, "scan setup dirs:%d %s", len(req.Dirs), setupDur)
	}
	started = time.Now()

	// max scandeps time in chromium/linux all build on P920 is 12s
	// as of 2023-06-26
	// but we see some timeout with 20s on linux-build-perf-developer builder
	// as of 2023-08-31 b/298142575, 2023-10-23 b/307202429
	// it was introduced to mitigate scanning that does not terminate,
	// but we see such scan recently, so set sufficient large timeout
	// to avoid scan failure due to timed out.
	const scanTimeout = 150 * time.Second

	icnt := 0
	ncnt := 0
	for scanner.hasInputs() {
		icnt++
		dur := time.Since(started)
		if dur > scanTimeout {
			return nil, fmt.Errorf("too slow scandeps: dirs:%d ds:%d i:%d n:%d %s %s", len(req.Dirs), scanner.maxDirstack, icnt, ncnt, setupDur, dur)
		}
		names := scanner.nextInputs(ctx)
		if log.V(1) {
			logNames := names
			clog.Infof(ctx, "try include %q", logNames)
		}
		for _, name := range names {
			ncnt++
			incpath, err := scanner.find(ctx, name)
			if err != nil {
				if errors.Is(err, ctx.Err()) {
					return nil, fmt.Errorf("timeout dirs:%d ds:%d i:%d n:%d %s %s: %w", len(req.Dirs), scanner.maxDirstack, icnt, ncnt, setupDur, time.Since(started), err)
				}
				if log.V(2) {
					lv := struct {
						name string
						err  error
					}{name, err}
					clog.Infof(ctx, "name %s not found: %v", lv.name, lv.err)
				}
				continue
			}
			if incpath == "" {
				// already read?
				continue
			}
			if log.V(1) {
				clog.Infof(ctx, "include %s -> %s", name, incpath)
			}
			if deps, ok := s.inputDeps[incpath]; ok {
				if log.V(1) {
					logDeps := deps
					clog.Infof(ctx, "add inputDeps %q", logDeps)
				}
				scanner.addInputs(ctx, deps...)
			}
			// TODO: check name in precomputed subtrees (i.e. sysroots etc)?
			// if not found, fallback to `clang -M`?
		}
	}
	results := scanner.results()
	return results, nil
}
