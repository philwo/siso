// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package hashfs provides a filesystem with digest hash.
package hashfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/golang/glog"
	"golang.org/x/sync/errgroup"

	"infra/build/siso/o11y/clog"
	"infra/build/siso/o11y/iometrics"
	"infra/build/siso/o11y/trace"
	"infra/build/siso/reapi/digest"
	"infra/build/siso/reapi/merkletree"
	"infra/build/siso/sync/semaphore"
)

var FlushSemaphore = semaphore.New("fs-flush", runtime.NumCPU()*2)

func localDigest(ctx context.Context, fname string, m *iometrics.IOMetrics) (digest.Data, error) {
	// TODO(b/271059955): add xattr support.
	src := digest.LocalFileSource{Fname: fname, IOMetrics: m}
	return digest.FromLocalFile(ctx, src)
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

// HashFS is a filesystem for digest hash.
type HashFS struct {
	opt       Option
	directory *directory

	// IOMetrics stores the metrics of I/O operations on the HashFS.
	IOMetrics *iometrics.IOMetrics
}

// New creates a HashFS.
func New(ctx context.Context, opt Option) (*HashFS, error) {
	if opt.DataSource == nil {
		opt.DataSource = noDataSource{}
	}
	fsys := &HashFS{
		opt:       opt,
		directory: &directory{},
		IOMetrics: iometrics.New("fs"),
	}
	if opt.StateFile != "" {
		fstate, err := Load(ctx, opt.StateFile)
		if err != nil {
			clog.Warningf(ctx, "Failed to load fs state from %s: %v", opt.StateFile, err)
		} else {
			clog.Infof(ctx, "Load fs state from %s", opt.StateFile)
			if err := fsys.SetState(ctx, fstate); err != nil {
				return nil, err
			}
		}
	}
	return fsys, nil
}

// Close closes the HashFS.
// Persists current state in opt.StateFile.
func (hfs *HashFS) Close(ctx context.Context) error {
	clog.Infof(ctx, "fs close")
	if hfs.opt.StateFile == "" {
		return nil
	}
	err := Save(ctx, hfs.opt.StateFile, hfs.State(ctx))
	if err != nil {
		clog.Errorf(ctx, "Failed to save fs state in %s: %v", hfs.opt.StateFile, err)
		return err
	}
	clog.Infof(ctx, "Saved fs state in %s", hfs.opt.StateFile)
	return nil
}

// FileSystem returns FileSystem interface at dir.
func (hfs *HashFS) FileSystem(ctx context.Context, dir string) FileSystem {
	return FileSystem{
		hashFS: hfs,
		ctx:    ctx,
		dir:    dir,
	}
}

// DataSource returns DataSource of the HashFS.
func (hfs *HashFS) DataSource() DataSource {
	return hfs.opt.DataSource
}

// Stat returns a FileInfo at root/fname.
func (hfs *HashFS) Stat(ctx context.Context, root, fname string) (*FileInfo, error) {
	if log.V(1) {
		clog.Infof(ctx, "stat @%s %s", root, fname)
	}
	fname = filepath.Join(root, fname)
	fname = filepath.ToSlash(fname)
	e, dir, ok := hfs.directory.Lookup(ctx, fname)
	if ok {
		err := e.WaitReady(ctx)
		if err != nil {
			return nil, fmt.Errorf("stat interrupted %s %s: %w", root, fname, err)
		}
		err = e.GetError()
		if err != nil {
			return nil, err
		}
		return &FileInfo{Fname: fname, E: e}, nil
	}
	e = newLocalEntry()
	if log.V(9) {
		clog.Infof(ctx, "store %s %s in %s", fname, e, dir)
	}
	var err error
	if dir != nil {
		err = dir.Store(ctx, filepath.Base(fname), e)
	} else {
		err = hfs.directory.Store(ctx, fname, e)
	}
	if err != nil {
		clog.Warningf(ctx, "failed to store %s %s in %s: %v", fname, e, dir, err)
		return nil, err
	}
	e.Init(ctx, fname, hfs.IOMetrics)
	clog.Infof(ctx, "stat new entry %s %s", fname, e)
	if err := e.GetError(); err != nil {
		return nil, err
	}
	return &FileInfo{Fname: fname, E: e}, nil
}

// ReadDir returns directory entries of root/name.
func (hfs *HashFS) ReadDir(ctx context.Context, root, name string) (dents []DirEntry, err error) {
	ctx, span := trace.NewSpan(ctx, "read-dir")
	defer span.Close(nil)
	if log.V(1) {
		clog.Infof(ctx, "readdir @%s %s", root, name)
		defer func() {
			clog.Infof(ctx, "readdir @%s %s -> %d %v", root, name, len(dents), err)
		}()
	}
	dirname := filepath.Join(root, name)
	dname := filepath.ToSlash(dirname)
	e, _, ok := hfs.directory.Lookup(ctx, dname)
	if !ok {
		e = newLocalEntry()
		err := hfs.directory.Store(ctx, dname, e)
		if err != nil {
			clog.Warningf(ctx, "failed to store %s %s: %v", dname, e, err)
			return nil, err
		}
		e.Init(ctx, dname, hfs.IOMetrics)
		clog.Infof(ctx, "stat new dir entry %s %s", dname, e)
	}
	err = e.GetError()
	if err != nil {
		return nil, fmt.Errorf("read dir %s: %w", dname, err)
	}
	names := e.UpdateDir(ctx, dirname)
	if e.Directory == nil {
		return nil, fmt.Errorf("read dir %s: not dir: %w", dname, os.ErrPermission)
	}
	if log.V(1) {
		clog.Infof(ctx, "update-dir %s -> %d", dirname, len(names))
	}
	if len(names) > 0 {
		// update from local dir entries.
		_, err = hfs.Entries(ctx, dirname, names)
		if err != nil {
			clog.Warningf(ctx, "readdir entries %s: %v", dirname, err)
		}
	}
	var ents []DirEntry
	e.Directory.M.Range(func(k, v any) bool {
		name := k.(string)
		ee := v.(*entry)
		ents = append(ents, DirEntry{
			Fi: &FileInfo{
				Fname: filepath.Join(root, dirname, name),
				E:     ee,
			},
		})
		return true
	})
	return ents, nil
}

// ReadFile reads a contents of root/fname.
func (hfs *HashFS) ReadFile(ctx context.Context, root, fname string) ([]byte, error) {
	ctx, span := trace.NewSpan(ctx, "read-file")
	defer span.Close(nil)
	if log.V(1) {
		clog.Infof(ctx, "readfile @%s %s", root, fname)
	}
	fname = filepath.Join(root, fname)
	fname = filepath.ToSlash(fname)
	span.SetAttr("fname", fname)
	e, _, ok := hfs.directory.Lookup(ctx, fname)
	if !ok {
		e = newLocalEntry()
		err := hfs.directory.Store(ctx, fname, e)
		if err != nil {
			clog.Warningf(ctx, "failed to store %s %s: %v", fname, e, err)
			return nil, err
		}
		e.Init(ctx, fname, hfs.IOMetrics)
		clog.Infof(ctx, "stat new entry %s %s", fname, e)
	}
	err := e.GetError()
	if err != nil {
		return nil, fmt.Errorf("read file %s: %w", fname, err)
	}
	if len(e.Buf) > 0 {
		return e.Buf, nil
	}
	err = e.WaitReady(ctx)
	if err != nil {
		return nil, err
	}
	if e.Data.IsZero() {
		return nil, fmt.Errorf("read file %s: no data", fname)
	}
	buf, err := digest.DataToBytes(ctx, e.Data)
	if log.V(1) {
		clog.Infof(ctx, "readfile %s: %v", fname, err)
	}
	return buf, err
}

// WriteFile writes a contents in root/fname with mtime and cmdhash.
func (hfs *HashFS) WriteFile(ctx context.Context, root, fname string, b []byte, isExecutable bool, mtime time.Time, cmdhash []byte) error {
	ctx, span := trace.NewSpan(ctx, "write-file")
	defer span.Close(nil)
	if log.V(1) {
		clog.Infof(ctx, "writefile @%s %s x:%t mtime:%s", root, fname, isExecutable, mtime)
	}
	data := digest.FromBytes(fname, b)
	fname = filepath.Join(root, fname)
	fname = filepath.ToSlash(fname)
	span.SetAttr("fname", fname)
	lready := make(chan bool, 1)
	lready <- true
	readyq := make(chan struct{})
	close(readyq)

	e := &entry{
		Lready:       lready,
		Mtime:        mtime,
		Readyq:       readyq,
		D:            data.Digest(),
		IsExecutable: isExecutable,
		Data:         data,
		Buf:          b,
		Cmdhash:      cmdhash,
	}
	e.Ready.Store(true)
	err := hfs.directory.Store(ctx, fname, e)
	clog.Infof(ctx, "writefile %s x:%t mtime:%s: %v", fname, isExecutable, mtime, err)
	return err
}

// Symlink creates a symlink to target at root/linkpath with mtime and cmdhash.
func (hfs *HashFS) Symlink(ctx context.Context, root, target, linkpath string, mtime time.Time, cmdhash []byte) error {
	if log.V(1) {
		clog.Infof(ctx, "symlink @%s %s -> %s", root, linkpath, target)
	}
	linkfname := filepath.Join(root, linkpath)
	linkfname = filepath.ToSlash(linkfname)
	lready := make(chan bool, 1)
	lready <- true
	readyq := make(chan struct{})
	close(readyq)
	e := &entry{
		Lready:  lready,
		Mtime:   mtime,
		Cmdhash: cmdhash,
		Readyq:  readyq,
		Target:  target,
	}
	e.Ready.Store(true)
	err := hfs.directory.Store(ctx, linkfname, e)
	clog.Infof(ctx, "symlink @%s %s -> %s: %v", root, linkpath, target, err)
	return err
}

// Copy copies a file from root/src to root/dst with mtime and cmdhash.
// if src is dir, returns error.
func (hfs *HashFS) Copy(ctx context.Context, root, src, dst string, mtime time.Time, cmdhash []byte) error {
	if log.V(1) {
		clog.Infof(ctx, "copy @%s %s to %s", root, src, dst)
	}
	srcname := filepath.Join(root, src)
	srcfname := filepath.ToSlash(srcname)
	dstfname := filepath.Join(root, dst)
	dstfname = filepath.ToSlash(dstfname)
	e, _, ok := hfs.directory.Lookup(ctx, srcfname)
	if !ok {
		e = newLocalEntry()
		if log.V(9) {
			clog.Infof(ctx, "new entry for copy src %s", srcfname)
		}
		err := hfs.directory.Store(ctx, srcfname, e)
		if err != nil {
			clog.Warningf(ctx, "failed to store copy src %s: %v", srcfname, err)
			return err
		}
		e.Init(ctx, srcfname, hfs.IOMetrics)
		clog.Infof(ctx, "copy src new entry %s %s", srcfname, e)
		if err := e.GetError(); err != nil {
			return err
		}
	}
	err := e.WaitReady(ctx)
	if err != nil {
		return err
	}
	subdir := e.GetDir()
	if subdir != nil {
		return fmt.Errorf("is a directory: %s", srcfname)
	}
	lready := make(chan bool, 1)
	lready <- true
	readyq := make(chan struct{})
	close(readyq)
	newEnt := &entry{
		Lready:       lready,
		Mtime:        mtime,
		Cmdhash:      cmdhash,
		Readyq:       readyq,
		D:            e.D,
		IsExecutable: e.IsExecutable,
		Target:       e.Target,
		// use the same data source as src if any.
		Data: e.Data,
		Buf:  e.Buf,
	}
	newEnt.Ready.Store(true)
	err = hfs.directory.Store(ctx, dstfname, newEnt)
	if err != nil {
		return err
	}
	clog.Infof(ctx, "copy %s to %s", srcfname, dstfname)
	return nil
}

// Mkdir makes a directory at root/dirname.
func (hfs *HashFS) Mkdir(ctx context.Context, root, dirname string) error {
	if log.V(1) {
		clog.Infof(ctx, "mkdir @%s %s", root, dirname)
	}
	dirname = filepath.Join(root, dirname)
	dirname = filepath.ToSlash(dirname)
	err := os.MkdirAll(dirname, 0755)
	hfs.IOMetrics.OpsDone(err)
	if err != nil {
		return err
	}
	lready := make(chan bool, 1)
	lready <- true
	readyq := make(chan struct{})
	close(readyq)

	e := &entry{
		Lready:    lready,
		Mtime:     time.Now(),
		Readyq:    readyq,
		Directory: &directory{},
	}
	e.Ready.Store(true)
	err = hfs.directory.Store(ctx, dirname, e)
	clog.Infof(ctx, "mkdir %s: %v", dirname, err)
	return err
}

// Remove removes a file at root/fname.
func (hfs *HashFS) Remove(ctx context.Context, root, fname string) error {
	if log.V(1) {
		clog.Infof(ctx, "remove @%s %s", root, fname)
	}
	fname = filepath.Join(root, fname)
	fname = filepath.ToSlash(fname)
	e, _, ok := hfs.directory.Lookup(ctx, fname)
	if !ok {
		e = newLocalEntry()
		e.SetError(os.ErrNotExist)
		err := hfs.directory.Store(ctx, fname, e)
		clog.Infof(ctx, "remove %s: %v", fname, err)
	}
	e.SetError(os.ErrNotExist)
	clog.Infof(ctx, "remove %s: %v", fname, nil)
	return nil
}

// Forget forgets cached entry for inputs under root.
func (hfs *HashFS) Forget(ctx context.Context, root string, inputs []string) {
	for _, fname := range inputs {
		fullname := filepath.Join(root, fname)
		fullname = filepath.ToSlash(fullname)
		hfs.directory.Delete(ctx, fullname)
	}
}

// LocalEntries gets merkletree entries for inputs at root from local disk,
// ignoring cached entries.
func (hfs *HashFS) LocalEntries(ctx context.Context, root string, inputs []string) ([]merkletree.Entry, error) {
	ctx, span := trace.NewSpan(ctx, "fs-local-entries")
	defer span.Close(nil)
	var entries []merkletree.Entry
	for _, fname := range inputs {
		fullname := filepath.Join(root, fname)
		fullname = filepath.ToSlash(fullname)
		fi, err := os.Lstat(fullname)
		hfs.IOMetrics.OpsDone(err)
		if err != nil {
			clog.Warningf(ctx, "failed to stat local-entry %s: %v", fullname, err)
			continue
		}
		switch {
		case fi.IsDir():
			entries = append(entries, merkletree.Entry{
				Name: fname,
			})
			continue
		case fi.Mode().Type() == fs.ModeSymlink:
			target, err := os.Readlink(fullname)
			hfs.IOMetrics.OpsDone(err)
			if err != nil {
				clog.Warningf(ctx, "failed to readlink %s: %v", fullname, err)
				continue
			}
			entries = append(entries, merkletree.Entry{
				Name:   fname,
				Target: target,
			})
			continue
		case fi.Mode().IsRegular():
			data, err := localDigest(ctx, fullname, hfs.IOMetrics)
			if err != nil {
				clog.Warningf(ctx, "failed to get diget %s: %v", fullname, err)
				continue
			}
			entries = append(entries, merkletree.Entry{
				Name:         fname,
				Data:         data,
				IsExecutable: isExecutable(fi, fullname),
			})
		default:
			clog.Warningf(ctx, "unexpected filetype %s: %s", fullname, fi.Mode())
			continue
		}
	}
	return entries, nil
}

// Entries gets merkletree entries for inputs at root.
func (hfs *HashFS) Entries(ctx context.Context, root string, inputs []string) ([]merkletree.Entry, error) {
	ctx, span := trace.NewSpan(ctx, "fs-entries")
	defer span.Close(nil)
	var nwait int
	var wg sync.WaitGroup
	ents := make([]*entry, 0, len(inputs))
	for _, fname := range inputs {
		fname := filepath.Join(root, fname)
		fname = filepath.ToSlash(fname)
		e, _, ok := hfs.directory.Lookup(ctx, fname)
		if ok {
			if log.V(2) {
				clog.Infof(ctx, "tree cache hit %s", fname)
			}
			ents = append(ents, e)
			if !e.Ready.Load() {
				wg.Add(1)
				nwait++
				go func() {
					defer wg.Done()
					select {
					case <-e.Readyq:
					case <-ctx.Done():
					}
				}()
			}
			continue
		}
		e = newLocalEntry()
		clog.Infof(ctx, "tree new entry %s", fname)
		if err := hfs.directory.Store(ctx, fname, e); err != nil {
			return nil, err
		}
		ents = append(ents, e)
		wg.Add(1)
		nwait++
		go func() {
			defer wg.Done()
			e.Init(ctx, fname, hfs.IOMetrics)
		}()
	}
	_, wspan := trace.NewSpan(ctx, "fs-entries-wait")
	wg.Wait()
	wspan.SetAttr("waits", nwait)
	wspan.Close(nil)
	var entries []merkletree.Entry
	for i, fname := range inputs {
		e := ents[i]
		if err := e.GetError(); err != nil || (e.Data.IsZero() && e.Target == "" && e.Directory == nil) {
			// TODO: hard fail instead?
			clog.Warningf(ctx, "missing %s data:%v target:%q: %v", fname, e.Data, e.Target, err)
			continue
		}
		data := e.Data
		isExecutable := e.IsExecutable
		target := e.Target
		if target != "" {
			// Linux imposes a limit of at most 40 symlinks in any one path lookup.
			// see: https://lwn.net/Articles/650786/
			const maxSymlinks = 40
			var tname string
			name := filepath.Join(root, fname)
			elink := e
			for j := 0; j < maxSymlinks; j++ {
				tname = filepath.Join(filepath.Dir(name), elink.Target)
				if log.V(1) {
					clog.Infof(ctx, "symlink %s -> %s", name, tname)
				}
				tname = filepath.ToSlash(tname)
				if strings.HasPrefix(tname, root+"/") {
					break
				}
				// symlink to out of exec root (e.g. ../.cipd/pkgs/..)
				name = tname
				tname = ""
				var ok bool
				elink, _, ok = hfs.directory.Lookup(ctx, name)
				if ok {
					if log.V(2) {
						clog.Infof(ctx, "tree cache hit %s", name)
					}
				} else {
					elink = newLocalEntry()
					clog.Infof(ctx, "tree new entry %s", name)
					if err := hfs.directory.Store(ctx, name, elink); err != nil {
						return nil, err
					}
					elink.Init(ctx, name, hfs.IOMetrics)
				}
				select {
				case <-elink.Readyq:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				if elink.Target == "" {
					break
				}
			}
			if e != elink {
				clog.Infof(ctx, "resolve symlink %s to %s", fname, name)
				target = elink.Target
				data = elink.Data
				isExecutable = elink.IsExecutable
			}
		}
		entries = append(entries, merkletree.Entry{
			Name:         fname,
			Data:         data,
			IsExecutable: isExecutable,
			Target:       target,
		})
	}
	return entries, nil
}

// Update updates cache information for entries under execRoot with mtime and cmdhash.
func (hfs *HashFS) Update(ctx context.Context, execRoot string, entries []merkletree.Entry, mtime time.Time, cmdhash []byte, action digest.Digest) error {
	ctx, span := trace.NewSpan(ctx, "fs-update")
	defer span.Close(nil)
	for _, ent := range entries {
		fname := filepath.Join(execRoot, ent.Name)
		fname = filepath.ToSlash(fname)
		switch {
		case !ent.Data.IsZero():
			lready := make(chan bool, 1)
			lready <- true
			readyq := make(chan struct{})
			close(readyq)
			e := &entry{
				Lready:       lready,
				Mtime:        mtime,
				Cmdhash:      cmdhash,
				Action:       action,
				Readyq:       readyq,
				D:            ent.Data.Digest(),
				IsExecutable: ent.IsExecutable,
				Data:         ent.Data,
			}
			e.Ready.Store(true)
			if err := hfs.directory.Store(ctx, fname, e); err != nil {
				return err
			}
		case ent.Target != "":
			lready := make(chan bool, 1)
			lready <- true
			readyq := make(chan struct{})
			close(readyq)
			e := &entry{
				Lready:  lready,
				Mtime:   mtime,
				Cmdhash: cmdhash,
				Action:  action,
				Readyq:  readyq,
				Target:  ent.Target,
			}
			e.Ready.Store(true)
			if err := hfs.directory.Store(ctx, fname, e); err != nil {
				return err
			}
		default: // directory
			lready := make(chan bool, 1)
			lready <- true
			readyq := make(chan struct{})
			close(readyq)
			e := &entry{
				Lready:    lready,
				Mtime:     mtime,
				Readyq:    readyq,
				Directory: &directory{},
			}
			e.Ready.Store(true)
			if err := hfs.directory.Store(ctx, fname, e); err != nil {
				return err
			}
			err := os.Chtimes(fname, time.Now(), mtime)
			hfs.IOMetrics.OpsDone(err)
			if err != nil {
				clog.Warningf(ctx, "failed to update dir mtime %s: %v", fname, err)
			}
		}
	}
	return nil
}

type noSource struct {
	filename string
}

func (ns noSource) Open(ctx context.Context) (io.ReadCloser, error) {
	return nil, fmt.Errorf("no source for %s", ns.filename)
}

func (ns noSource) String() string {
	return fmt.Sprintf("noSource:%s", ns.filename)
}

type noDataSource struct{}

func (noDataSource) DigestData(d digest.Digest, fname string) digest.Data {
	return digest.NewData(noSource{fname}, d)
}

// Flush flushes cached information for files under execRoot to local disk.
func (hfs *HashFS) Flush(ctx context.Context, execRoot string, files []string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ctx, span := trace.NewSpan(ctx, "flush")
	defer span.Close(nil)
	eg, ctx := errgroup.WithContext(ctx)
	for _, file := range files {
		fname := filepath.Join(execRoot, file)
		fname = filepath.ToSlash(fname)
		e, _, ok := hfs.directory.Lookup(ctx, fname)
		if !ok {
			// If it doesn't exist in memory, just use local disk as is.
			continue
		}
		select {
		case need := <-e.Lready:
			if !need {
				if log.V(1) {
					clog.Infof(ctx, "flush %s local ready", fname)
				}
				err := e.GetError()
				if errors.Is(err, fs.ErrNotExist) {
					clog.Warningf(ctx, "flush %s local-ready: %v", fname, err)
					continue
				}
				if err != nil {
					return fmt.Errorf("flush %s local-ready: %w", fname, err)
				}
				continue
			}
		case <-ctx.Done():
			return fmt.Errorf("flush %s: %w", fname, ctx.Err())
		}

		if !e.Ready.Load() {
			// digest etc is not yet ready,
			// which means it is calculating digest from local disk
			// so we can assume local disk is ready.
			continue
		}
		ctx, done, err := FlushSemaphore.WaitAcquire(ctx)
		if err != nil {
			return fmt.Errorf("flush %s: %w", fname, err)
		}
		eg.Go(func() error {
			defer done()
			return e.Flush(ctx, fname, hfs.IOMetrics)
		})
	}
	return eg.Wait()
}

// Refresh refreshes cached file entries under execRoot.
func (hfs *HashFS) Refresh(ctx context.Context, execRoot string) error {
	// XXX: optimize?
	state := hfs.State(ctx)
	return hfs.SetState(ctx, state)
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

// TODO(b/266518906): remove this.
type entry = Entry

func newLocalEntry() *Entry {
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
	case fi.Mode().Type() == fs.ModeSymlink:
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
	// M is a map of file in a directoy's basename to *entry.
	M sync.Map
}

// TODO(b/266518906): remove this.
type directory = Directory

func (d *directory) String() string {
	if d == nil {
		return "<nil>"
	}
	// better to dump all entries?
	return fmt.Sprintf("&directory{m:%p}", &d.M)
}

// TODO(b/266518906): make this private.
func (d *directory) Lookup(ctx context.Context, fname string) (*entry, *directory, bool) {
	origFname := fname
	for fname != "" {
		fname = strings.TrimPrefix(fname, "/")
		elem, rest, ok := strings.Cut(fname, "/")
		if !ok {
			e, ok := d.M.Load(fname)
			if !ok {
				return nil, d, ok
			}
			return e.(*entry), d, ok
		}
		fname = rest
		v, ok := d.M.Load(elem)
		var subdir *directory
		if ok {
			e := v.(*entry)
			subdir = e.GetDir()
			if subdir == nil {
				err := e.WaitReady(ctx)
				if err != nil {
					clog.Warningf(ctx, "lookup %s wait failed %v", origFname, err)
				} else {
					subdir = e.GetDir()
				}
			}
		}
		if subdir == nil {
			log.V(1).Infof("lookup %s subdir %s %s -> %s", origFname, elem, fname, subdir)
			return nil, nil, false
		}
		d = subdir
	}
	log.V(1).Infof("lookup %s fname empty", origFname)
	return nil, nil, false
}

// TODO(b/266518906): make this private.
func (d *directory) Store(ctx context.Context, fname string, e *entry) error {
	origFname := fname
	if log.V(8) {
		clog.Infof(ctx, "store %s %v", origFname, e)
	}
	for fname != "" {
		fname = strings.TrimPrefix(fname, "/")
		elem, rest, ok := strings.Cut(fname, "/")
		if !ok {
			v, ok := d.M.Load(fname)
			var ee *entry
			if ok {
				ee = v.(*entry)
			}
			if ok && e.Directory != nil {
				ee.InitDir()
			} else {
				if ok && ee != nil {
					cmdchanged := !bytes.Equal(ee.Cmdhash, e.Cmdhash)
					if e.Target != "" && ee.Target != e.Target {
						clog.Infof(ctx, "store %s: cmdchagne:%t s:%q to %q", origFname, cmdchanged, ee.Target, e.Target)
					} else if !e.D.IsZero() && ee.D != e.D && ee.D.SizeBytes != 0 && e.D.SizeBytes != 0 {
						// don't log nil to digest of empty file (size=0)
						clog.Infof(ctx, "store %s: cmdchange:%t d:%v to %v", origFname, cmdchanged, ee.D, e.D)
					}
				}
				d.M.Store(fname, e)
			}
			if log.V(8) {
				clog.Infof(ctx, "store %s -> %s %s", origFname, d, fname)
			}
			return nil
		}
		fname = rest
		v, ok := d.M.Load(elem)
		if ok {
			dent := v.(*entry)
			err := dent.WaitReady(ctx)
			if err != nil {
				return fmt.Errorf("store interrupted %s: %w", origFname, err)
			}
			if dent.GetError() == nil {
				d = dent.GetDir()
				if log.V(9) {
					clog.Infof(ctx, "store %s subdir0 %s %s -> %s (%v)", origFname, elem, fname, d, dent)
				}
				if d == nil {
					return fmt.Errorf("failed to set entry: %s not dir %#v", elem, dent)
				}
				continue
			}
		}
		// create intermediate dir of elem.
		lready := make(chan bool, 1)
		lready <- true
		readyq := make(chan struct{})
		close(readyq)
		newDent := &entry{
			Lready: lready,
			// don't set mtime for intermediate dir.
			// mtime will be updated by updateDir
			// when all dirents have been loaded.
			Readyq:    readyq,
			Directory: &directory{},
		}
		newDent.Ready.Store(true)
		dent := newDent
		v, ok = d.M.LoadOrStore(elem, dent)
		if ok {
			dent = v.(*entry)
		}
		subdir := dent.GetDir()
		err := dent.WaitReady(ctx)
		if err != nil {
			return fmt.Errorf("store interrupted %s: %w", origFname, err)
		}
		err = dent.GetError()
		if err != nil {
			// local was known to not exist. replace it with new dent.
			if log.V(9) {
				clog.Infof(ctx, "store %s subdir-update %s %s -> %s (%v)", origFname, elem, fname, subdir, dent)
			}
			dent.InitDir()
			subdir = dent.GetDir()
		}
		if log.V(9) {
			clog.Infof(ctx, "store %s subdir1 %s %s -> %s (%v)", origFname, elem, fname, subdir, dent)
		}
		d = subdir
		if d == nil {
			return fmt.Errorf("failed to set entry: %s not dir %#v", elem, dent)
		}

	}
	return fmt.Errorf("bad fname? %q", origFname)
}

// TODO(b/266518906): make this private.
func (d *directory) Delete(ctx context.Context, fname string) {
	for fname != "" {
		fname = strings.TrimPrefix(fname, "/")
		elem, rest, ok := strings.Cut(fname, "/")
		if !ok {
			d.M.Delete(fname)
			return
		}
		fname = rest
		v, ok := d.M.Load(elem)
		if !ok {
			return
		}
		e := v.(*entry)
		d = e.GetDir()
		if d == nil {
			return
		}
	}
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
