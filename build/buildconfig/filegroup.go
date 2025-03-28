// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package buildconfig

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strings"

	log "github.com/golang/glog"

	"infra/build/siso/o11y/clog"
)

// Filegroups is serialized filegoups information for cache.
type Filegroups struct {
	// Etags are opaque identifiers map by label.
	// Each etag is assigned to specific version of a filegroup
	// for the label.
	ETags map[string]string `json:"etags"`

	// Filegroups are filegroup's map by label.
	// It can be used to ninjabuild.InputDeps as is.
	Filegroups map[string][]string `json:"filegroups"`
}

// filegroup is a filegoup information for cache.
type filegroup struct {
	etag  string
	files []string
}

// filegroupUpdater updates a filegroup.
type filegroupUpdater interface {
	// Update updates a filegroup.
	// It returns original filegroup if etag matches.
	// Otherwise, it returns a new filegroup.
	Update(context.Context, fs.FS, filegroup) (filegroup, error)
}

// update this when glob behavior has been changed to regenerate
// filegroup even if ninja build or globSpec don't change.
const globVer = "glob.0"

// globSpec specifies glob operations.
type globSpec struct {
	dir      string
	includes []string
	excludes []string
}

// Update updates a filegroup.
// It returns original filegroup if etag matches.
// Otherwise, it scans fsys and returns a filegroup matched with the glob spec.
func (g globSpec) Update(ctx context.Context, fsys fs.FS, fg filegroup) (filegroup, error) {
	hash := g.hash()
	if fg.etag == hash {
		return fg, nil
	}
	fsys, err := fs.Sub(fsys, g.dir)
	if err != nil {
		return filegroup{}, err
	}
	m := g.matcher()
	fg.etag = hash
	var files []string
	err = fs.WalkDir(fsys, ".", func(pathname string, d fs.DirEntry, err error) error {
		if log.V(1) {
			clog.Infof(ctx, "glob %s dir=%t: %v", pathname, d.IsDir(), err)
		}
		if err != nil {
			return err
		}
		if d.IsDir() {
			return err
		}
		pathname = filepath.ToSlash(pathname)
		if m(pathname) {
			files = append(files, path.Join(g.dir, pathname))
		}
		return nil
	})
	sort.Strings(files)
	fg.files = files
	return fg, err
}

func (g globSpec) hash() string {
	h := sha256.New()
	fmt.Fprintf(h, "dir:%s\n", g.dir)
	sort.Strings(g.includes)
	fmt.Fprintf(h, "includes:%q\n", g.includes)
	sort.Strings(g.excludes)
	fmt.Fprintf(h, "excludes:%q\n", g.excludes)
	return globVer + "/" + hex.EncodeToString(h.Sum(nil))
}

func (g globSpec) matcher() func(string) bool {
	var inc, exc []func(string) bool
	for _, p := range g.includes {
		p := p
		if strings.Contains(p, "/") {
			inc = append(inc, func(s string) bool {
				ok, _ := path.Match(p, s)
				return ok
			})
			continue
		}
		inc = append(inc, func(s string) bool {
			ok, _ := path.Match(p, path.Base(s))
			return ok
		})
	}
	for _, p := range g.excludes {
		p := p
		if strings.Contains(p, "/") {
			exc = append(exc, func(s string) bool {
				ok, _ := path.Match(p, s)
				return ok
			})
			continue
		}
		exc = append(exc, func(s string) bool {
			ok, _ := path.Match(p, path.Base(s))
			return ok
		})
	}
	return func(s string) bool {
		for _, m := range exc {
			match := m(s)
			if match {
				return false
			}
		}
		for _, m := range inc {
			match := m(s)
			if match {
				return true
			}
		}
		return false
	}
}
