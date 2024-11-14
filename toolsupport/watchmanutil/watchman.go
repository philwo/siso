// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package watchmanutil

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	pb "infra/build/siso/hashfs/proto"
	"infra/build/siso/o11y/clog"
)

const clockFilename = ".siso_watchman_clock"

// Watchman interacts with watchman.
// https://facebook.github.io/watchman/
type Watchman struct {
	watchmanPath string
	dir          string

	// watchman clock of the last checked.
	clock watchClock
	// list reported by `watchman since`.
	since watchSince
	// `watchman since` data keyed by filename.
	m map[string]*watchSinceFile
}

// New creates watchman for dir by watchman binary at watchmanPath.
// User needs to install `watchman` in PATH,
// and run `watchman watch-project $dir` before using it.
func New(ctx context.Context, watchmanPath, dir string) (*Watchman, error) {
	w := &Watchman{
		watchmanPath: watchmanPath,
		dir:          dir,
	}
	err := w.check(ctx)
	if err != nil {
		return nil, err
	}
	buf, err := os.ReadFile(clockFilename)
	if err != nil {
		clog.Warningf(ctx, "failed to read %s: %v", clockFilename, err)
	} else {
		err = json.Unmarshal(buf, &w.clock)
		if err != nil {
			return nil, fmt.Errorf("failed to parse clock %s: %w", clockFilename, err)
		}
		clog.Infof(ctx, "watchman last clock: %q", buf)
	}
	return w, nil
}

// Version returns version of watchman.
func (w *Watchman) Version() string {
	if w.clock.Clock != "" {
		return fmt.Sprintf("%s\n  version=%q last clock=%q", w.watchmanPath, w.clock.Version, w.clock.Clock)
	}
	return fmt.Sprintf("%s\n  version=%q no last clock", w.watchmanPath, w.clock.Version)
}

type watchListData struct {
	Version string   `json:"version"`
	Roots   []string `json:"roots"`
}

func (w *Watchman) check(ctx context.Context) error {
	var wl watchListData
	buf, err := exec.CommandContext(ctx, w.watchmanPath, "watch-list").Output()
	if err != nil {
		return err
	}
	err = json.Unmarshal(buf, &wl)
	if err != nil {
		return fmt.Errorf("failed to parse watch-list %q: %w", buf, err)
	}
	clog.Infof(ctx, "watchman version: %s", wl.Version)
	w.clock.Version = wl.Version
	dir := filepath.Clean(w.dir)
	for i := range wl.Roots {
		if filepath.Clean(wl.Roots[i]) == dir {
			return nil
		}
	}
	return fmt.Errorf("dir %s is not watched %q", dir, wl.Roots)
}

type watchClock struct {
	Version string `json:"version"`
	Clock   string `json:"clock"`
}

// Close closes watchman.
// it saves current clock in `.siso_watchman_clock`.
func (w *Watchman) Close(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, w.watchmanPath, "clock", w.dir).Output()
	if err != nil {
		return err
	}
	clog.Infof(ctx, "watchman save clock: %q", out)
	return os.WriteFile(clockFilename, out, 0644)
}

type watchSinceFile struct {
	Name   string `json:"name"`
	Exists bool   `json:"exists"`
	Size   int64  `json:"size"`
	Mode   int    `json:"mode"`
	Mtime  int64  `json:"mtime"`
	Nlink  int    `json:"nlink"`
}

type watchSince struct {
	Version string           `json:"version"`
	Clock   string           `json:"clock"`
	Files   []watchSinceFile `json:"files"`
}

// Scan calls `watchman since` to finds all files that were modified.
func (w *Watchman) Scan(ctx context.Context) error {
	if w.since.Version != "" && w.since.Clock != "" {
		w.clock.Version = w.since.Version
		w.clock.Clock = w.since.Clock
	}
	if w.clock.Version == "" || w.clock.Clock == "" {
		return fmt.Errorf("no watchman last clock")
	}
	started := time.Now()
	// `watchman since` returns many files and may take time to parse the result.
	// we might want to exclude patterns that are known not to be used for build?
	out, err := exec.CommandContext(ctx, w.watchmanPath, "since", w.dir, w.clock.Clock).Output()
	if err != nil {
		return err
	}
	err = json.Unmarshal(out, &w.since)
	if err != nil {
		return fmt.Errorf("failed to parse watchman since: %w", err)
	}
	clog.Infof(ctx, "watchman since %s->%s %d files in %s", w.clock.Clock, w.since.Clock, len(w.since.Files), time.Since(started))
	w.m = make(map[string]*watchSinceFile)
	for i := range w.since.Files {
		fname := filepath.ToSlash(filepath.Join(w.dir, w.since.Files[i].Name))
		w.m[fname] = &w.since.Files[i]
	}
	return nil
}

type FileInfo struct {
	ent *pb.Entry
}

// Name returns base name of the file.
func (fi FileInfo) Name() string {
	return filepath.Base(fi.ent.Name)
}

// Size returns length in bytes.
func (fi FileInfo) Size() int64 {
	return fi.ent.GetDigest().GetSizeBytes()
}

// Mode returns file mode bits.
func (fi FileInfo) Mode() fs.FileMode {
	m := fs.FileMode(0644)
	if fi.ent.IsExecutable {
		m |= 0111
	}
	if fi.ent.Target != "" {
		m |= fs.ModeSymlink
	}
	if fi.IsDir() {
		m |= fs.ModeDir | 0111
	}
	return m
}

// ModTime returns modification time.
func (fi FileInfo) ModTime() time.Time {
	return time.Unix(0, fi.ent.GetId().GetModTime())
}

// IsDir returns abbreviation for Mode().IsDir().
func (fi FileInfo) IsDir() bool {
	return fi.ent.Target == "" && fi.ent.Digest.GetHash() == ""
}

// Sys returns underlying data source (*pb.Entry).
func (fi FileInfo) Sys() any {
	return fi.ent
}

// FileInfo returns file info for the entry.
func (w *Watchman) FileInfo(ctx context.Context, ent *pb.Entry) (fs.FileInfo, error) {
	f, ok := w.m[ent.Name]
	if !ok {
		// ent.Name is not modified since last build.
		// we can use the entry as is.
		return FileInfo{ent: ent}, nil
	}
	if !f.Exists {
		return FileInfo{}, fs.ErrNotExist
	}
	// watchman uses unix time for mtime, no subsecond granularity...
	return os.Lstat(ent.Name)
}
