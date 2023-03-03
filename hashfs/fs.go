// Copyright 2023 The Chromium Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package hashfs provides a filesystem with digest hash.
package hashfs

import (
	"context"
	"io/fs"
	"path/filepath"
	"time"

	"infra/build/siso/reapi/digest"
	"infra/build/siso/reapi/merkletree"
)

// TODO(b/266518906): make this struct and its fields private.
type Entry struct {
	Mtime time.Time

	D            digest.Digest
	IsExecutable bool
	Target       string // symlink

	Data digest.Data // from local
}

type HashFS struct {
}

// FileInfo implements https://pkg.go.dev/io/fs#FileInfo.
type FileInfo struct {
	// TODO(b/266518906): make these fields private.
	Fname string
	E     *Entry
}

// Name is a base name of the file.
func (fi *FileInfo) Name() string {
	return filepath.Base(fi.Fname)
}

// Size is a size of the file.
func (fi *FileInfo) Size() int64 {
	if fi.E.D.IsZero() {
		return 0
	}
	return fi.E.D.SizeBytes
}

// Mode is a file mode of the file.
func (fi *FileInfo) Mode() fs.FileMode {
	mode := fs.FileMode(0644)
	if fi.E.D.IsZero() && fi.E.Target == "" {
		mode |= fs.ModeDir
	} else if fi.E.D.IsZero() && fi.E.Target != "" {
		mode |= fs.ModeSymlink
	}
	if fi.E.IsExecutable {
		mode |= 0111
	}
	return mode
}

// ModTime is a modification time of the file.
func (fi *FileInfo) ModTime() time.Time {
	return fi.E.Mtime
}

// IsDir returns true if it is the directory.
func (fi *FileInfo) IsDir() bool {
	// TODO: e.directory != nil?
	return fi.E.D.IsZero() && fi.E.Target == ""
}

// Sys returns merkletree Entry of the file.
func (fi *FileInfo) Sys() any {
	return merkletree.Entry{
		Name:         fi.Fname,
		Data:         fi.E.Data,
		IsExecutable: fi.E.IsExecutable,
		Target:       fi.E.Target,
	}
}

// DirEntry implements https://pkg.go.dev/io/fs#DirEntry.
type DirEntry struct {
	// TODO(b/266518906): make this private.
	Fi *FileInfo
}

// Name is a base name in the directory.
func (de DirEntry) Name() string {
	return de.Fi.Name()
}

// IsDir returns true if it is a directory.
func (de DirEntry) IsDir() bool {
	return de.Fi.IsDir()
}

// Type returns a file type.
func (de DirEntry) Type() fs.FileMode {
	return de.Fi.Mode().Type()
}

// Info returns a FileInfo.
func (de DirEntry) Info() (fs.FileInfo, error) {
	return de.Fi, nil
}

// Entries gets merkletree entries for inputs at root.
func (hfs *HashFS) Entries(ctx context.Context, root string, inputs []string) ([]merkletree.Entry, error) {
	return nil, nil
}

// Forget forgets cached entry for inputs under root.
func (hfs *HashFS) Forget(ctx context.Context, root string, inputs []string) {
}
