// Copyright 2023 The Chromium Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package hashfs

import (
	"io"
	"io/fs"
)

// File implements https://pkg.go.dev/io/fs#File.
// This is an in-memory representation of the file.
type File struct {
	// TODO(b/266518906): make these fields private.
	Buf []byte
	Fi  *FileInfo
}

// Stat returns a FileInfo describing the file.
func (f *File) Stat() (fs.FileInfo, error) {
	return f.Fi, nil
}

// Read reads contents from the file.
func (f *File) Read(buf []byte) (int, error) {
	if len(f.Buf) == 0 {
		return 0, io.EOF
	}
	n := copy(buf, f.Buf)
	f.Buf = f.Buf[n:]
	return n, nil
}

// Close closes the file.
func (f *File) Close() error {
	return nil
}

// Dir implements https://pkg.go.dev/io/fs#ReadDirFile.
type Dir struct {
	// TODO(b/266518906): make these fields private.
	Ents []DirEntry
	Fi   *FileInfo
}

// Stat returns a FileInfo describing the directory.
func (d *Dir) Stat() (fs.FileInfo, error) {
	return d.Fi, nil
}

// Read reads contents from the dir (permission denied).
func (d *Dir) Read(buf []byte) (int, error) {
	return 0, fs.ErrPermission
}

// Close closes the directory.
func (d *Dir) Close() error {
	return nil
}

// ReadDir reads directory entries from the dir.
// TODO(b/271363619): return at most n entries.
func (d *Dir) ReadDir(n int) ([]fs.DirEntry, error) {
	if n <= 0 {
		var ents []fs.DirEntry
		for _, e := range d.Ents {
			ents = append(ents, e)
		}
		d.Ents = nil
		return ents, nil
	}
	var ents []fs.DirEntry
	var i int
	var e DirEntry
	for i, e = range d.Ents {
		ents = append(ents, e)
	}
	d.Ents = d.Ents[i:]
	if len(d.Ents) == 0 {
		return ents, io.EOF
	}
	return ents, nil
}
