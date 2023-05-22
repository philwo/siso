// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package scandeps

import (
	"context"
	"io/fs"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"infra/build/siso/o11y/trace"
	"infra/build/siso/sync/semaphore"
)

var cppScanSema = semaphore.New("cppscan", runtime.NumCPU())

// fsview is a view of filesystem per scandeps process.
// It will reduce unnecessary contention to filesystem.
type fsview struct {
	fs        *filesystem
	execRoot  string
	inputDeps map[string][]string

	sysroots []string

	searchPaths []string

	files map[string]*scanResult

	// scanned filenames for results.
	visited map[string]bool

	// TODO(b/282888305) implement this
}

func (fv *fsview) addDir(ctx context.Context, dir string, searchPath bool) {
	if _, ok := fv.inputDeps[dir+":headers"]; ok {
		// use precomputed subtree for this directory,
		// so no need to handle this dir.
		return
	}
	// TODO(b/282888305) cache dirents in dir.
	for _, sysinc := range fv.sysroots {
		if dir == sysinc || strings.HasPrefix(dir, sysinc+"/") {
			// use precomputed subtree (sysroot)
			// for this directory, so no need to handle this dir.
			return
		}
	}
	if searchPath {
		// dir may be added to dir stack, but not in searchPaths yet?
		seen := false
		for _, p := range fv.searchPaths {
			if dir == p {
				seen = true
				break
			}
		}
		if !seen {
			fv.searchPaths = append(fv.searchPaths, dir)
		}
	}
	fv.visited[dir] = true
}

func (fv *fsview) get(ctx context.Context, dir, name string) (string, *scanResult, error) {
	// TODO(b/282888305) use cached dirents etc.
	incpath := filepath.Join(dir, name)
	if !filepath.IsLocal(incpath) {
		return "", nil, fs.ErrNotExist
	}
	incpath = filepath.ToSlash(incpath)
	if v, ok := fv.visited[incpath]; ok {
		if !v {
			return "", nil, fs.ErrNotExist
		}
	}
	sr, err := fv.scanFile(ctx, incpath)
	if err != nil {
		fv.visited[incpath] = false
		return "", nil, err
	}
	fv.visited[incpath] = true
	return incpath, sr, err
}

func (fv *fsview) scanFile(ctx context.Context, fname string) (*scanResult, error) {
	sr, err := fv.scanResult(ctx, fname)
	if err != nil {
		return sr, err
	}
	sr.mu.Lock()
	defer sr.mu.Unlock()
	if sr.done {
		return sr, sr.err
	}
	ctx, span := trace.NewSpan(ctx, "scanFile")
	defer span.Close(nil)

	buf, err := fv.fs.hashfs.ReadFile(ctx, fv.execRoot, fname)
	if err != nil {
		return sr, sr.err
	}
	var includes []string
	var defines map[string][]string
	err = cppScanSema.Do(ctx, func(ctx context.Context) error {
		var err error
		includes, defines, err = cppScan(ctx, fname, buf)
		return err
	})
	sr.err = err
	sr.includes = make([]string, 0, len(includes))
	for _, incname := range includes {
		// TODO(b/282888305) intern incname to share string content.
		sr.includes = append(sr.includes, incname)
	}
	sr.defines = make(map[string][]string, len(defines))
	for k, v := range defines {
		// TODO(b/282888305) intern k to share string content.
		values := make([]string, 0, len(v))
		for _, val := range v {
			// TODO(b/282888305) intern val to share string content.
			values = append(values, val)
		}
		sr.defines[k] = values
	}
	sr.done = true
	return sr, sr.err
}

func (fv *fsview) scanResult(ctx context.Context, fname string) (*scanResult, error) {
	sr, ok := fv.getFile(fname)
	if ok {
		if sr == nil {
			return nil, fs.ErrNotExist
		}
		return sr, nil
	}
	// TODO(b/282888305) optimize file access
	fi, err := fv.fs.hashfs.Stat(ctx, fv.execRoot, fname)
	if err != nil {
		fv.setFile(fname, nil)
		return nil, fs.ErrNotExist
	}
	if fi.Mode().IsDir() {
		fv.setFile(fname, nil)
		return nil, fs.ErrInvalid
	}
	if !fi.Mode().IsRegular() {
		fv.setFile(fname, nil)
		return nil, fs.ErrInvalid
	}
	sr = &scanResult{}
	fv.setFile(fname, sr)
	return sr, nil
}

func (fv *fsview) getFile(fname string) (*scanResult, bool) {
	// TODO(b/282888305) use shared *scanResult in *filesystem
	sr, ok := fv.files[fname]
	return sr, ok
}

func (fv *fsview) setFile(fname string, sr *scanResult) {
	fv.files[fname] = sr
	// TODO(b/282888305) share in *filesystem
}

func (fv *fsview) results() []string {
	results := make([]string, 0, len(fv.visited))
	for k, v := range fv.visited {
		if k == "" {
			continue
		}
		if strings.Contains(k, ":") {
			continue
		}
		if !v {
			continue
		}
		results = append(results, k)
	}
	sort.Strings(results)
	return results
}
