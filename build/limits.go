// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"infra/build/siso/o11y/clog"
	"infra/build/siso/ui"
)

const (
	// limit # of concurrent steps at most 1024 times of num cpus
	// to protect from out of memory, or too many threads.
	stepLimitFactor = 1024

	// limit # of concurrent scandeps steps at most 2 times of num cpus
	// to protect from out of memory, reduce contention
	scanDepsLimitFactor = 2

	// limit # of concurrent steps at most 80 times of num cpus
	// to protect from out of memory, or DDoS to RE API.
	remoteLimitFactor = 80
)

// Limits specifies the resource limits used in siso build process.
// zero limit means default.
type Limits struct {
	Step      int
	Preproc   int
	ScanDeps  int
	Local     int
	FastLocal int
	Remote    int
	REWrap    int
	Cache     int
}

var (
	limitOnce     sync.Once
	defaultLimits Limits
)

// DefaultLimits returns default semaphore limits.
// It checks SISO_LIMITS environment variable to override limits.
// SISO_LIMITS is comma-separated <key>=<value> pair.
// e.g.
//
//	SISO_LIMITS=step=1024,local=8,remote=80
func DefaultLimits(ctx context.Context) Limits {
	limitOnce.Do(func() {
		numCPU := runtime.NumCPU()
		defaultLimits = Limits{
			Step:      stepLimitFactor * numCPU,
			Preproc:   stepLimitFactor * numCPU,
			ScanDeps:  scanDepsLimitFactor * numCPU,
			Local:     numCPU,
			FastLocal: limitForFastLocal(ctx, numCPU),
			Remote:    remoteLimitFactor * numCPU,
			REWrap:    limitForREWrapper(ctx, numCPU),
			Cache:     stepLimitFactor * numCPU,
		}
		overrides := os.Getenv("SISO_LIMITS")
		if overrides == "" {
			return
		}
		for _, ov := range strings.Split(overrides, ",") {
			ov = strings.TrimSpace(ov)
			clog.Infof(ctx, "apply SISO_LIMITS=%s", ov)
			k, v, ok := strings.Cut(ov, "=")
			if !ok {
				clog.Warningf(ctx, "wrong SISO_LIMITS value %q", ov)
				continue
			}
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 || (n == 0 && k != "fastlocal") {
				clog.Warningf(ctx, "wrong limits value for %s: %v", k, v)
				continue
			}
			switch k {
			case "step":
				defaultLimits.Step = n
			case "preproc":
				defaultLimits.Preproc = n
			case "scandeps":
				defaultLimits.ScanDeps = n
			case "local":
				defaultLimits.Local = n
			case "fastlocal":
				defaultLimits.FastLocal = n
			case "remote":
				defaultLimits.Remote = n
			case "rewrap":
				defaultLimits.REWrap = n
			case "cache":
				defaultLimits.Cache = n
			default:
				clog.Warningf(ctx, "unknown limits name %q", k)
				continue
			}
			ui.Default.PrintLines(ui.SGR(ui.Yellow, fmt.Sprintf("use SISO_LIMITS=%s=%d\n", k, n)))
		}
	})
	return defaultLimits
}

// UnitTestLimits returns limits used in unit tests.
// It sets 2 for all limits.
// Otherwise, builder will start many steps, so hard to
// test !hasReady before b.failuresAllowed in Build func in builder.go
func UnitTestLimits(ctx context.Context) Limits {
	clog.Infof(ctx, "UnitTest mode. limit to 2")
	return Limits{
		Step:     2,
		Preproc:  2,
		ScanDeps: 2,
		Local:    2,
		Remote:   2,
		REWrap:   2,
		Cache:    2,
	}
}

// SetDefaultForTest updates default limits for test.
// Test should restore the original value after the test.
func SetDefaultForTest(limits Limits) {
	defaultLimits = limits
}

func limitForFastLocal(ctx context.Context, numCPU int) int {
	if !isLocalFilesystem(ctx) {
		// If it is not on local filesystem (e.g. Cog),
		// local execution may be slow as it is not fast to
		// access file locally.
		// Prefer remote than local on non local filesystem.
		return 0
	}
	// We want to use local resources on powerful machine (but not so
	// many, as it needs to run local only steps too),
	// but not want to use on cheap machine (*-standard-8 etc).
	// So don't use fast local if cpus < 32.
	// 64 cpus -> 2
	// 128 cpus -> 6
	return max(0, numCPU-32) / 16
}

func limitForREWrapper(ctx context.Context, numCPU int) int {
	// same logic in depot_tools/autoninja.py
	// https://chromium.googlesource.com/chromium/tools/depot_tools.git/+/54762c22175e17dce4f4eab18c5942c06e82478f/autoninja.py#166
	const defaultCoreMultiplier = remoteLimitFactor
	coreMultiplier := defaultCoreMultiplier
	if v := os.Getenv("NINJA_CORE_MULTIPLIER"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil {
			clog.Warningf(ctx, "wrong $NINJA_CORE_MULTIPLIER=%q; %v", v, err)
		} else {
			coreMultiplier = p
		}
	}
	if runtime.GOARCH == "amd64" {
		// autoninja half num_cores for platform.machine is 'x86_64' or 'AMD64'.
		numCPU /= 2
		if numCPU == 0 {
			numCPU = 1
		}
	}
	limit := numCPU * coreMultiplier
	if v := os.Getenv("NINJA_CORE_LIMIT"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil {
			clog.Warningf(ctx, "wrong $NINJA_CORE_LIMIT=%q; %v", v, err)
		} else if limit > p {
			limit = p
		}
	}
	switch runtime.GOOS {
	case "windows":
		// on Windows, higher than 1000 does not improve build
		// performance, but may cause namedpipe timeout
		// b/70640154 b/223211029
		if limit > 1000 {
			limit = 1000
		}
	case "darwin":
		// on macOS, higher than 800 causes 'Too many open files' error
		// (crbug.com/936864).
		if limit > 800 {
			limit = 800
		}
	}
	return limit
}
