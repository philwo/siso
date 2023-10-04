// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"fmt"
	"path/filepath"
	"sync"

	log "github.com/golang/glog"
)

// Path manages paths used by the build.
type Path struct {
	ExecRoot string
	Dir      string // relative to ExecRoot

	// Symbol table for seen paths.
	intern symtab
	// Stores paths converted cwd relative to exec root relative.
	m sync.Map
}

// NewPath returns new path for the build.
func NewPath(execRoot, dir string) *Path {
	return &Path{
		ExecRoot: execRoot,
		Dir:      dir,
	}
}

// Check checks the path is valid.
func (p *Path) Check() error {
	if !filepath.IsAbs(p.ExecRoot) {
		return fmt.Errorf("exec_root must be absolute path: %q", p.ExecRoot)
	}
	if filepath.IsAbs(p.Dir) {
		return fmt.Errorf("dir must be relative to exec_root: %q", p.Dir)
	}
	return nil
}

// Intern interns the path.
func (p *Path) Intern(path string) string {
	return p.intern.Intern(path)
}

// MustFromWD converts cwd relative to exec root relative.
// TODO(b/273185597): consider rewriting as non-Must func.
func (p *Path) MustFromWD(path string) string {
	s, err := p.FromWD(path)
	if err != nil {
		log.Errorf("Failed to get rel %s, %s: %v", p.ExecRoot, path, err)
		return path
	}
	return s
}

// FromWD converts cwd relative to exec root relative,
// slash-separated.
// It keeps absolute path if it is out of exec root.
func (p *Path) FromWD(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	v, ok := p.m.Load(path)
	if ok {
		return v.(string), nil
	}
	if filepath.IsAbs(path) {
		rel, err := filepath.Rel(p.ExecRoot, path)
		if err != nil {
			return "", err
		}
		if !filepath.IsLocal(rel) {
			// use abs path for out of exec root
			return path, nil
		}
		rel = filepath.ToSlash(rel)
		rel = p.intern.Intern(rel)
		v, _ = p.m.LoadOrStore(path, rel)
		return v.(string), nil
	}
	s := filepath.ToSlash(filepath.Join(p.Dir, path))
	s = p.intern.Intern(s)
	v, _ = p.m.LoadOrStore(path, s)
	return v.(string), nil
}

// MustToWD converts exec root relative to cwd relative,
// slash-separated.
// It keeps absolute path as is.
// TODO(b/273185597): consider rewriting as non-Must func.
func (p *Path) MustToWD(path string) string {
	if path == "" {
		return ""
	}
	if filepath.IsAbs(path) {
		return path
	}
	rel, err := filepath.Rel(p.Dir, path)
	if err != nil {
		log.Errorf("Failed to get rel %s, %s: %v", p.Dir, path, err)
		return path
	}
	rel = filepath.ToSlash(rel)
	return rel
}

// AbsFromWD converts cwd relative to absolute path.
func (p *Path) AbsFromWD(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(p.ExecRoot, p.Dir, path)
}
