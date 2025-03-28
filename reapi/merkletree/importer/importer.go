// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package importer is an importer of directory tree into RBE-CAS.
package importer

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"go.chromium.org/infra/build/siso/hashfs/osfs"
	"go.chromium.org/infra/build/siso/reapi/digest"
	"go.chromium.org/infra/build/siso/reapi/merkletree"
)

// Importer is an importer.
type Importer struct{}

// Import imports dir into digest store and returns digest of root.
func (Importer) Import(ctx context.Context, dir string, ds *digest.Store) (digest.Digest, error) {
	var entries []merkletree.Entry
	// TODO: pass osfs from subcommand?
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if dir == path {
			return nil
		}
		name := strings.TrimPrefix(path, dir+"/")
		if d.IsDir() {
			entries = append(entries, merkletree.Entry{
				Name: name,
			})
			return nil
		}
		if d.Type()&fs.ModeSymlink == fs.ModeSymlink {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			entries = append(entries, merkletree.Entry{
				Name:   name,
				Target: target,
			})
			return nil
		}
		if d.Type()&fs.ModeType != 0 {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		data, err := digest.FromSource(ctx, osfs.NewFileSource(path))
		if err != nil {
			return err
		}
		entries = append(entries, merkletree.Entry{
			Name:         name,
			Data:         data,
			IsExecutable: (fi.Mode()&fs.ModePerm)&0111 != 0,
		})
		return nil
	})
	if err != nil {
		return digest.Digest{}, err
	}

	inputTree := merkletree.New(ds)
	for _, ent := range entries {
		err = inputTree.Set(ent)
		if err != nil {
			return digest.Digest{}, err
		}
	}
	return inputTree.Build(ctx)
}
