// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package hashfs

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"syscall"
)

// File implements https://pkg.go.dev/io/fs#File.
// This is an in-memory representation of the file.
type File struct {
	buf []byte
	fi  FileInfo
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
	fi   FileInfo
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
func (d *Dir) ReadDir(n int) ([]fs.DirEntry, error) {
	if n <= 0 {
		var ents []fs.DirEntry
		for _, e := range d.ents {
			ents = append(ents, e)
		}
		d.ents = nil
		return ents, nil
	}
	if len(d.ents) == 0 {
		return nil, io.EOF
	}
	var i int
	var e DirEntry
	var ents []fs.DirEntry
	for i, e = range d.ents {
		if i == n {
			break
		}
		ents = append(ents, e)
	}
	d.ents = d.ents[len(ents):]
	return ents, nil
}

// FileSystem provides fs.{FS,ReadDirFS,ReadFileFS,StatFS,SubFS} interfaces.
type FileSystem struct {
	hashFS *HashFS
	ctx    context.Context
	dir    string
}

// Open opens a file for name.
func (fsys FileSystem) Open(name string) (fs.File, error) {
	fi, err := fsys.hashFS.Stat(fsys.ctx, fsys.dir, name)
	if err != nil {
		return nil, &fs.PathError{
			Op:   "open",
			Path: name,
			Err:  err,
		}
	}
	if fi.IsDir() {
		ents, err := fsys.hashFS.ReadDir(fsys.ctx, fsys.dir, name)
		if err != nil {
			return nil, &fs.PathError{
				Op:   "open",
				Path: name,
				Err:  err,
			}
		}
		return &Dir{
			ents: ents,
			fi:   fi,
		}, nil
	}
	buf, err := fsys.hashFS.ReadFile(fsys.ctx, fsys.dir, name)
	if err != nil {
		return nil, &fs.PathError{
			Op:   "open",
			Path: name,
			Err:  err,
		}
	}
	return &File{
		buf: buf,
		fi:  fi,
	}, nil
}

// ReadDir reads directory at name.
func (fsys FileSystem) ReadDir(name string) ([]fs.DirEntry, error) {
	ents, err := fsys.hashFS.ReadDir(fsys.ctx, fsys.dir, name)
	if err != nil {
		return nil, &fs.PathError{
			Op:   "readdir",
			Path: name,
			Err:  err,
		}
	}
	dirents := make([]fs.DirEntry, 0, len(ents))
	for _, e := range ents {
		dirents = append(dirents, e)
	}
	return dirents, nil
}

// ReadFile reads contents of name.
func (fsys FileSystem) ReadFile(name string) ([]byte, error) {
	buf, err := fsys.hashFS.ReadFile(fsys.ctx, fsys.dir, name)
	if err != nil {
		return nil, &fs.PathError{
			Op:   "readfile",
			Path: name,
			Err:  err,
		}
	}
	return buf, nil
}

// Stat gets stat of name.
func (fsys FileSystem) Stat(name string) (fs.FileInfo, error) {
	fi, err := fsys.hashFS.Stat(fsys.ctx, fsys.dir, name)
	if err != nil {
		return nil, &fs.PathError{
			Op:   "stat",
			Path: name,
			Err:  err,
		}
	}
	return fi, nil
}

// Sub returns an FS corresponding to the subtree rooted at dir.
func (fsys FileSystem) Sub(dir string) (fs.FS, error) {
	origDir := dir
	for i := 0; i < maxSymlinks; i++ {
		fi, err := fsys.Stat(dir)
		if err != nil {
			return nil, err
		}
		if !fi.IsDir() {
			if hfi, ok := fi.(FileInfo); ok && hfi.Target() != "" {
				target := hfi.Target()
				if filepath.IsAbs(target) {
					return nil, &fs.PathError{
						Op:   "sub",
						Path: origDir,
						Err:  fmt.Errorf("symlink to abs path %s", target),
					}
				}
				dir = filepath.Join(filepath.Dir(dir), target)
				continue
			}
			return nil, &fs.PathError{
				Op:   "sub",
				Path: origDir,
				Err:  fmt.Errorf("not directory: %s", dir),
			}
		}
		return FileSystem{
			hashFS: fsys.hashFS,
			ctx:    fsys.ctx,
			dir:    filepath.Join(fsys.dir, dir),
		}, nil
	}
	return nil, &fs.PathError{
		Op:   "sub",
		Path: origDir,
		Err:  syscall.ELOOP,
	}
}
