// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package buildconfig

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"go.starlark.net/starlark"

	"infra/build/siso/build"
	"infra/build/siso/hashfs"
	"infra/build/siso/o11y/clog"
)

func parseFilegroups(ctx context.Context, v starlark.Value) (map[string]filegroupUpdater, error) {
	d, ok := v.(*starlark.Dict)
	if !ok {
		return nil, fmt.Errorf("filegroups %T, want dict", v)
	}
	fg := map[string]filegroupUpdater{}
	for _, k := range d.Keys() {
		key, ok := starlark.AsString(k)
		if !ok {
			return nil, fmt.Errorf("filegroups key %T (%v), want string", k, k)
		}
		val, _, err := d.Get(k)
		if err != nil {
			return nil, fmt.Errorf("filegroups value for %s: %w", key, err)
		}
		g, err := parseFilegroupUpdater(ctx, key, val)
		if err != nil {
			return nil, fmt.Errorf("filegroups for %s: %w", key, err)
		}
		fg[key] = g
	}
	return fg, nil
}

func parseFilegroupUpdater(ctx context.Context, key string, v starlark.Value) (filegroupUpdater, error) {
	d, ok := v.(*starlark.Dict)
	if !ok {
		return nil, fmt.Errorf("filegroup %T, want dict", v)
	}
	typeval, _, err := d.Get(starlark.String("type"))
	if err != nil {
		return nil, fmt.Errorf("filegroup type: %w", err)
	}
	tv, ok := starlark.AsString(typeval)
	if !ok {
		return nil, fmt.Errorf("filegroup type %T, want string", typeval)
	}
	switch tv {
	case "glob":
		var dir string
		dirval, found, err := d.Get(starlark.String("dir"))
		if !found {
			dir = key
			i := strings.Index(dir, ":")
			if i >= 0 {
				dir = dir[:i]
			}
		} else {
			if err != nil {
				return nil, fmt.Errorf("filegroup glob dir: %w", err)
			}
			var ok bool
			dir, ok = starlark.AsString(dirval)
			if !ok {
				return nil, fmt.Errorf("filegroup glob dir %T, want string", dirval)
			}
		}
		includesVal, _, err := d.Get(starlark.String("includes"))
		if err != nil {
			return nil, fmt.Errorf("filegroup glob includes: %w", err)
		}
		includes, err := unpackList(includesVal)
		if err != nil {
			return nil, fmt.Errorf("filegroup glob includes: %w", err)
		}
		excludesVal, ok, err := d.Get(starlark.String("excludes"))
		if err != nil {
			return nil, fmt.Errorf("filegroup glob excludes: %w", err)
		}
		var excludes []string
		if ok {
			excludes, err = unpackList(excludesVal)
			if err != nil {
				return nil, fmt.Errorf("filegroup glob excludes: %w", err)
			}
		}
		return globSpec{
			dir:      dir,
			includes: includes,
			excludes: excludes,
		}, nil
	default:
		return nil, fmt.Errorf("filegroup type=%q; not supported", tv)
	}
}

// UpdateFilegroups returns updated filegroups.
// It takes cached filegroups that were generated by the previous build.
// When a given filegroup is valid, it is reused.
// Otherwise, gets the file list by the filegroup operation. e.g. glob.
func (cfg *Config) UpdateFilegroups(ctx context.Context, hashFS *hashfs.HashFS, buildPath *build.Path, filegroups Filegroups) (Filegroups, error) {
	fsys := hashFS.FileSystem(ctx, buildPath.ExecRoot)
	fg := Filegroups{
		ETags:      make(map[string]string),
		Filegroups: make(map[string][]string),
	}
	for k, g := range cfg.filegroups {
		clog.Infof(ctx, "filegroup %s", k)
		v := filegroup{
			etag:  filegroups.ETags[k],
			files: filegroups.Filegroups[k],
		}
		v, err := g.Update(ctx, fsys, v)
		if errors.Is(err, fs.ErrNotExist) {
			// ignore the filegroup. b/283203079
			continue
		}
		if err != nil {
			return Filegroups{}, err
		}
		fg.ETags[k] = v.etag
		fg.Filegroups[k] = v.files
	}
	return fg, nil
}
