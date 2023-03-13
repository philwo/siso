// Copyright 2023 The Chromium Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package hashfs provides a filesystem with digest hash.
package hashfs

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/golang/glog"

	"infra/build/siso/o11y/clog"
	"infra/build/siso/o11y/iometrics"
	"infra/build/siso/reapi/digest"
	"infra/build/siso/reapi/merkletree"
)

func localDigest(ctx context.Context, fname string, m *iometrics.IOMetrics) (digest.Data, error) {
	// TODO(b/271059955): add xattr support.
	src := digest.LocalFileSource{Fname: fname, IOMetrics: m}
	return digest.FromLocalFile(ctx, src)
}

func isSymlink(m fs.FileMode) bool {
	return m.Type() == fs.ModeSymlink
}

func isExecutable(fi fs.FileInfo, fname string) bool {
	if fi.Mode()&0111 != 0 {
		return true
	}
	if runtime.GOOS != "windows" {
		return false
	}
	// siso-toolchain-chromium-browser-clang creates an executables
	// marker file, so check it.
	_, err := os.Stat(fname + ".is_executable")
	return err == nil
}

// TODO(b/266518906): make this struct and its fields private.
type Entry struct {
	// Lready represents whether it is ready to use local file.
	// true - need to download contents.
	// block - download is in progress.
	// closed/false - file is already downloaded.
	Lready chan bool

	Mtime time.Time

	// cmdhash is hash of command lines that generated this file.
	// e.g. hash('touch output.stamp')
	Cmdhash []byte

	// digest of action that generated this file.
	Action digest.Digest

	// Readyq represents whether it is ready to use file metadatas below.
	// block - calculate in progress.
	// closed - already available. readyq has been closed.
	Readyq chan struct{}
	// atomic flag for readiness of metadata.
	// true - ready. readyq was closed.
	// false - not ready. need to wait on readyq.
	Ready        atomic.Bool
	D            digest.Digest
	IsExecutable bool
	Target       string // symlink.

	Data digest.Data // from local.
	Buf  []byte      // from WriteFile.

	Mu        sync.RWMutex
	Directory *Directory
	Err       error
}

// TODO(b/266518906): make this private.
func NewLocalEntry() *Entry {
	lready := make(chan bool)
	close(lready)
	readyq := make(chan struct{})
	return &Entry{
		Lready: lready,
		Readyq: readyq,
	}
}

func (e *Entry) String() string {
	if e.Ready.Load() {
		switch {
		case e.GetDir() != nil:
			return "<directory>"
		case e.Target != "":
			return "<symlink>"
		default:
			return "<file>"
		}
	}
	return "<not ready>"
}

// TODO(b/266518906): make this private.
func (e *Entry) Init(ctx context.Context, fname string, m *iometrics.IOMetrics) {
	defer func() {
		close(e.Readyq)
		e.Ready.Store(true)
	}()
	fi, err := os.Lstat(fname)
	m.OpsDone(err)
	if errors.Is(err, fs.ErrNotExist) {
		clog.Infof(ctx, "not exist %s", fname)
		e.SetError(err)
		return
	}
	if err != nil {
		clog.Warningf(ctx, "failed to lstat %s: %v", fname, err)
		e.SetError(err)
		return
	}
	switch {
	case fi.IsDir():
		if log.V(1) {
			clog.Infof(ctx, "tree entry %s: is dir", fname)
		}
		e.InitDir()
		// don't update mtime, so updateDir scans local dir.
		return
	case isSymlink(fi.Mode()):
		e.Target, err = os.Readlink(fname)
		m.OpsDone(err)
		if err != nil {
			e.SetError(err)
		}
		if log.V(1) {
			clog.Infof(ctx, "tree entry %s: symlink to %s: %v", fname, e.Target, e.Err)
		}
	case fi.Mode().IsRegular():
		e.Data, err = localDigest(ctx, fname, m)
		if err != nil {
			clog.Errorf(ctx, "tree entry %s: file error: %v", fname, err)
			e.SetError(err)
			return
		}
		e.D = e.Data.Digest()
		e.IsExecutable = isExecutable(fi, fname)
		if log.V(1) {
			clog.Infof(ctx, "tree entry %s: file %s x:%t", fname, e.D, e.IsExecutable)
		}
	default:
		e.SetError(fmt.Errorf("unexpected filetype not regular %s: %s", fi.Mode(), fname))
		clog.Errorf(ctx, "tree entry %s: unknown filetype %s", fname, fi.Mode())
		return
	}
	if e.Mtime.Before(fi.ModTime()) {
		e.Mtime = fi.ModTime()
	}
}

// TODO(b/266518906): make this private.
func (e *Entry) WaitReady(ctx context.Context) error {
	if !e.Ready.Load() {
		select {
		case <-e.Readyq:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// TODO(b/266518906): make this private.
func (e *Entry) InitDir() {
	e.Mu.Lock()
	defer e.Mu.Unlock()
	if e.Directory == nil {
		e.Directory = &Directory{}
	}
	// reset mtime so updateDir will lookup local dir.
	e.Mtime = time.Time{}
	e.Err = nil
}

// TODO(b/266518906): make this private.
func (e *Entry) UpdateDir(ctx context.Context, dname string) []string {
	e.Mu.Lock()
	defer e.Mu.Unlock()
	d, err := os.Open(dname)
	if err != nil {
		clog.Warningf(ctx, "updateDir %s: open %v", dname, err)
		return nil
	}
	defer d.Close()
	fi, err := d.Stat()
	if err != nil {
		clog.Warningf(ctx, "updateDir %s: stat %v", dname, err)
		return nil
	}
	if !fi.IsDir() {
		clog.Warningf(ctx, "updateDir %s: is not dir?", dname)
		return nil
	}
	if fi.ModTime().Equal(e.Mtime) {
		if log.V(1) {
			clog.Infof(ctx, "updateDir %s: up-to-date %s", dname, e.Mtime)
		}
		return nil
	}
	names, err := d.Readdirnames(-1)
	if err != nil {
		clog.Warningf(ctx, "updateDir %s: readdirnames %v", dname, err)
		return nil
	}
	clog.Infof(ctx, "updateDir mtime %s %d %s", dname, len(names), e.Mtime)
	e.Mtime = fi.ModTime()
	return names
}

// GetDir returns directory of entry.
// TODO(b/266518906): make this private.
func (e *Entry) GetDir() *Directory {
	if e == nil {
		return nil
	}
	return e.Directory
}

// TODO(b/266518906): make this private.
func (e *Entry) SetError(err error) {
	e.Mu.Lock()
	defer e.Mu.Unlock()
	if e.Err == nil {
		e.Err = err
	}
	if e.Err != nil {
		e.D = digest.Digest{}
		e.Data = digest.Data{}
		e.Buf = nil
		e.Target = ""
		e.Directory = nil
	}
}

// TODO(b/266518906): make this private.
func (e *Entry) GetError() error {
	e.Mu.RLock()
	defer e.Mu.RUnlock()
	return e.Err
}

// TODO(b/266518906): make this private.
func (e *Entry) Flush(ctx context.Context, fname string, m *iometrics.IOMetrics) error {
	defer close(e.Lready)

	if errors.Is(e.GetError(), fs.ErrNotExist) {
		err := os.RemoveAll(fname)
		m.OpsDone(err)
		clog.Infof(ctx, "flush remove %s: %v", fname, err)
		return nil
	}
	switch {
	case e.Directory != nil:
		// directory
		err := os.MkdirAll(fname, 0755)
		m.OpsDone(err)
		clog.Infof(ctx, "flush dir %s: %v", fname, err)
		e.SetError(err)
		return err
	case e.D.IsZero() && e.Target != "":
		err := os.Symlink(e.Target, fname)
		m.OpsDone(err)
		if errors.Is(err, fs.ErrExist) {
			err = os.Remove(fname)
			m.OpsDone(err)
			err = os.Symlink(e.Target, fname)
			m.OpsDone(err)
		}
		clog.Infof(ctx, "flush symlink %s -> %s: %v", fname, e.Target, err)
		e.SetError(err)
		// don't change mtimes. it fails if target doesn't exist.
		return err
	default:
	}
	fi, err := os.Lstat(fname)
	m.OpsDone(err)
	if err == nil {
		if fi.Size() == e.D.SizeBytes && fi.ModTime().Equal(e.Mtime) {
			// TODO: check hash, mode?
			clog.Infof(ctx, "flush %s: already exist", fname)
			return nil
		}
		var fileDigest digest.Digest
		d, err := localDigest(ctx, fname, m)
		if err == nil {
			fileDigest = d.Digest()
			if fileDigest == e.D {
				clog.Infof(ctx, "flush %s: already exist - hash match", fname)
				if !fi.ModTime().Equal(e.Mtime) {
					err = os.Chtimes(fname, time.Now(), e.Mtime)
					m.OpsDone(err)
				}
				return err
			}
		}
		clog.Warningf(ctx, "flush %s: exists but mismatch size:%d!=%d mtime:%s!=%s d:%v!=%v", fname, fi.Size(), e.D.SizeBytes, fi.ModTime(), e.Mtime, fileDigest, e.D)
		if fi.Mode()&0200 == 0 {
			// need to be writable. otherwise os.WriteFile fails with permission denied.
			err = os.Chmod(fname, fi.Mode()|0200)
			m.OpsDone(err)
			clog.Warningf(ctx, "flush %s: not writable? %s: %v", fname, fi.Mode(), err)
		}

	}
	err = os.MkdirAll(filepath.Dir(fname), 0755)
	m.OpsDone(err)
	if err != nil {
		clog.Warningf(ctx, "flush %s: mkdir: %v", fname, err)
		return fmt.Errorf("failed to create directory for %s: %w", fname, err)
	}
	if e.D.SizeBytes == 0 {
		clog.Infof(ctx, "flush %s: empty file", fname)
		err := os.WriteFile(fname, nil, 0644)
		m.WriteDone(0, err)
		if err != nil {
			e.SetError(err)
			return err
		}
		err = os.Chtimes(fname, time.Now(), e.Mtime)
		m.OpsDone(err)
		if err != nil {
			e.SetError(err)
			return err
		}
		return nil
	}
	buf := e.Buf
	if len(buf) == 0 {
		if e.Data.IsZero() {
			return fmt.Errorf("no data: retrieve %s: ", fname)
		}
		buf, err = digest.DataToBytes(ctx, e.Data)
		clog.Infof(ctx, "flush %s %s from source: %v", fname, e.D, err)
		if err != nil {
			e.SetError(err)
			return fmt.Errorf("flush %s size=%d: %w", fname, e.D.SizeBytes, err)
		}
	} else {
		clog.Infof(ctx, "flush %s from embedded buf", fname)
	}
	mode := os.FileMode(0644)
	if e.IsExecutable {
		mode = os.FileMode(0755)
	}
	err = os.WriteFile(fname, buf, mode)
	m.WriteDone(len(buf), err)
	if err != nil {
		e.SetError(err)
		return err
	}
	err = os.Chtimes(fname, time.Now(), e.Mtime)
	m.OpsDone(err)
	if err != nil {
		e.SetError(err)
		return err
	}
	return nil
}

// directory is per-directory entry map to reduce mutex contention.
// TODO: use generics as DirMap<K,V>?
// TODO(b/266518906): make this struct and its fields private.
type Directory struct {
	// M is a map of file's basename to *entry.
	M sync.Map
}

func (d *Directory) String() string {
	if d == nil {
		return "<nil>"
	}
	// better to dump all entries?
	return fmt.Sprintf("&directory{m:%p}", &d.M)
}

type HashFS struct {
}

// DataSource returns DataSource of the HashFS.
func (hfs *HashFS) DataSource() DataSource {
	return nil
}

// Entries gets merkletree entries for inputs at root.
func (hfs *HashFS) Entries(ctx context.Context, root string, inputs []string) ([]merkletree.Entry, error) {
	return nil, nil
}

// Forget forgets cached entry for inputs under root.
func (hfs *HashFS) Forget(ctx context.Context, root string, inputs []string) {
}

// Update updates cache information for entries under execRoot with mtime and cmdhash.
func (hfs *HashFS) Update(ctx context.Context, execRoot string, entries []merkletree.Entry, mtime time.Time, cmdhash []byte, action digest.Digest) error {
	return nil
}

// FileInfo implements https://pkg.go.dev/io/fs#FileInfo.
type FileInfo struct {
	// Fname is full path of file.
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

// CmdHash returns a cmdhash that created the file.
func (fi *FileInfo) CmdHash() []byte {
	return fi.E.Cmdhash
}

// Action returns a digest of action that created the file.
func (fi *FileInfo) Action() digest.Digest {
	return fi.E.Action
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
