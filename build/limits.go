// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/charmbracelet/log"
	"go.chromium.org/infra/build/siso/ui"
)

const (
	// limit # of concurrent steps at most 512 times of num cpus
	// to protect from out of memory, or too many threads.
	stepLimitFactor = 512

	// limit # of concurrent steps at most 80 times of num cpus
	// to protect from out of memory, or DDoS to RE API.
	remoteLimitFactor = 80
)

// Limits specifies the resource limits used in siso build process.
// zero limit means default.
type Limits struct {
	Step       int
	Local      int
	StartLocal int
	Remote     int
	Thread     int
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
func DefaultLimits() Limits {
	limitOnce.Do(func() {
		numCPU := runtime.NumCPU()
		stepLimit := limitForStep(numCPU)
		defaultLimits = Limits{
			Step:   stepLimit,
			Local:  numCPU,
			Remote: limitForRemote(numCPU),
		}
		// On many cores machine, it would hit default max thread limit = 10000.
		// Usually, it would require 1/3 of stepLimit threads (cache miss case?).
		// For safe, sets 1/2 of stepLimit for max threads. b/325565625
		maxThreads := defaultLimits.Step / 2
		if maxThreads > 10000 {
			defaultLimits.Thread = maxThreads
		}
		overrides := os.Getenv("SISO_LIMITS")
		if overrides == "" {
			return
		}
		for _, ov := range strings.Split(overrides, ",") {
			ov = strings.TrimSpace(ov)
			log.Infof("apply SISO_LIMITS=%s", ov)
			k, v, ok := strings.Cut(ov, "=")
			if !ok {
				log.Warnf("wrong SISO_LIMITS value %q", ov)
				continue
			}
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 || n == 0 {
				log.Warnf("wrong limits value for %s: %v", k, v)
				continue
			}
			switch k {
			case "step":
				defaultLimits.Step = n
			case "local":
				defaultLimits.Local = n
			case "startlocal":
				defaultLimits.StartLocal = n
			case "remote":
				defaultLimits.Remote = n
			case "thread":
				defaultLimits.Thread = n
			default:
				log.Warnf("unknown limits name %q", k)
				continue
			}
			ui.Default.Warningf("use SISO_LIMITS=%s=%d", k, n)
		}
	})
	return defaultLimits
}

func limitForStep(numCPU int) int {
	return stepLimitFactor * numCPU
}

func limitForRemote(numCPU int) int {
	limit := remoteLimitFactor * numCPU
	// Intel Mac has bad performance with a large number of remote actions.
	if runtime.GOOS == "darwin" && runtime.GOARCH == "amd64" {
		return min(1000, limit)
	}
	return limit
}
