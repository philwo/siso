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
	buf []byte
	fi  *FileInfo
}

// Stat returns a FileInfo describing the file.
func (f *File) Stat() (fs.FileInfo, error) {
	return f.fi, nil
}

// Read reads contents from the file.
func (f *File) Read(buf []byte) (int, error) {
	if len(f.buf) == 0 {
		return 0, io.EOF
	}
	n := copy(buf, f.buf)
	f.buf = f.buf[n:]
	return n, nil
}

// Close closes the file.
func (f *File) Close() error {
	return nil
}

// Dir implements https://pkg.go.dev/io/fs#ReadDirFile.
type Dir struct {
	ents []DirEntry
	fi   *FileInfo
}

// Stat returns a FileInfo describing the directory.
func (d *Dir) Stat() (fs.FileInfo, error) {
	return d.fi, nil
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
		for _, e := range d.ents {
			ents = append(ents, e)
		}
		d.ents = nil
		return ents, nil
	}
	var ents []fs.DirEntry
	var i int
	var e DirEntry
	for i, e = range d.ents {
		ents = append(ents, e)
	}
	d.ents = d.ents[i:]
	if len(d.ents) == 0 {
		return ents, io.EOF
	}
	return ents, nil
}
