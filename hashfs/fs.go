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

type entry struct {
	mtime time.Time

	d            digest.Digest
	isExecutable bool
	target       string // symlink

	data digest.Data // from local
}

type HashFS struct {
}

// FileInfo implements https://pkg.go.dev/io/fs#FileInfo.
type FileInfo struct {
	fname string
	e     *entry
}

// Name is a base name of the file.
func (fi *FileInfo) Name() string {
	return filepath.Base(fi.fname)
}

// Size is a size of the file.
func (fi *FileInfo) Size() int64 {
	if fi.e.d.IsZero() {
		return 0
	}
	return fi.e.d.SizeBytes
}

// Mode is a file mode of the file.
func (fi *FileInfo) Mode() fs.FileMode {
	mode := fs.FileMode(0644)
	if fi.e.d.IsZero() && fi.e.target == "" {
		mode |= fs.ModeDir
	} else if fi.e.d.IsZero() && fi.e.target != "" {
		mode |= fs.ModeSymlink
	}
	if fi.e.isExecutable {
		mode |= 0111
	}
	return mode
}

// ModTime is a modification time of the file.
func (fi *FileInfo) ModTime() time.Time {
	return fi.e.mtime
}

// IsDir returns true if it is the directory.
func (fi *FileInfo) IsDir() bool {
	// TODO: e.directory != nil?
	return fi.e.d.IsZero() && fi.e.target == ""
}

// Sys returns merkletree Entry of the file.
func (fi *FileInfo) Sys() any {
	return merkletree.Entry{
		Name:         fi.fname,
		Data:         fi.e.data,
		IsExecutable: fi.e.isExecutable,
		Target:       fi.e.target,
	}
}

// DirEntry implements https://pkg.go.dev/io/fs#DirEntry.
type DirEntry struct {
	fi *FileInfo
}

// Name is a base name in the directory.
func (de DirEntry) Name() string {
	return de.fi.Name()
}

// IsDir returns true if it is a directory.
func (de DirEntry) IsDir() bool {
	return de.fi.IsDir()
}

// Type returns a file type.
func (de DirEntry) Type() fs.FileMode {
	return de.fi.Mode().Type()
}

// Info returns a FileInfo.
func (de DirEntry) Info() (fs.FileInfo, error) {
	return de.fi, nil
}

// Entries gets merkletree entries for inputs at root.
func (hfs *HashFS) Entries(ctx context.Context, root string, inputs []string) ([]merkletree.Entry, error) {
	return nil, nil
}

// Forget forgets cached entry for inputs under root.
func (hfs *HashFS) Forget(ctx context.Context, root string, inputs []string) {
}
