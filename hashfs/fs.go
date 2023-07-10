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

// Linux imposes a limit of at most 40 symlinks in any one path lookup.
// see: https://lwn.net/Articles/650786/
const maxSymlinks = 40

// FlushSemaphore is a semaphore to control concurrent flushes.
var FlushSemaphore = semaphore.New("fs-flush", runtime.NumCPU()*2)

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

// NotifyFunc is the type of the function to notify the filesystem changes.
type NotifyFunc func(context.Context, *FileInfo)

// HashFS is a filesystem for digest hash.
type HashFS struct {
	opt       Option
	directory *directory

	notifies []NotifyFunc

	// IOMetrics stores the metrics of I/O operations on the HashFS.
	IOMetrics *iometrics.IOMetrics

	digester digester
	clean    bool
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

		digester: digester{
			q:    make(chan digestReq, 1000),
			quit: make(chan struct{}),
			done: make(chan struct{}),
		},
	}
	if opt.StateFile != "" {
		start := time.Now()
		fstate, err := Load(ctx, opt.StateFile)
		if err != nil {
			clog.Warningf(ctx, "Failed to load fs state from %s: %v", opt.StateFile, err)
		} else {
			clog.Infof(ctx, "Load fs state from %s: %s", opt.StateFile, time.Since(start))
			if err := fsys.SetState(ctx, fstate); err != nil {
				return nil, err
			}
		}
	}
	go fsys.digester.start()
	return fsys, nil
}

// Notify causes the hashfs to relay filesystem motification to f.
func (hfs *HashFS) Notify(f NotifyFunc) {
	hfs.notifies = append(hfs.notifies, f)
}

// Close closes the HashFS.
// Persists current state in opt.StateFile.
func (hfs *HashFS) Close(ctx context.Context) error {
	clog.Infof(ctx, "fs close")
	hfs.digester.stop()
	if hfs.opt.StateFile == "" {
		return nil
	}
	if hfs.clean {
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

// IsClean returns whether hashfs is clean (i.e. sync with local disk).
func (hfs *HashFS) IsClean() bool {
	return hfs.clean
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

func needPathClean(names ...string) bool {
	for _, name := range names {
		// even on windows, we use /-path in hashfs.
		if strings.Contains(name, `\`) {
			return true
		}
		if strings.Contains(name, "//") {
			return true
		}
		i := strings.IndexByte(name, '.')
		if i < 0 {
			continue
		}
		name = name[i:]
		if strings.HasPrefix(name, "./") || strings.HasPrefix(name, "../") {
			return true
		}
	}
	return false
}

func (hfs *HashFS) dirLookup(ctx context.Context, root, fname string) (*entry, *directory, bool) {
	if needPathClean(root, fname) {
		return hfs.directory.lookup(ctx, hfs.directory, filepath.ToSlash(filepath.Join(root, fname)))
	}
	e, _, ok := hfs.directory.lookup(ctx, hfs.directory, root)
	if !ok {
		return nil, nil, false
	}
	if e.directory == nil {
		return nil, nil, false
	}
	return e.directory.lookup(ctx, hfs.directory, fname)
}

func (hfs *HashFS) dirStoreAndNotify(ctx context.Context, fullname string, e *entry) error {
	_, err := hfs.directory.store(ctx, fullname, e)
	if err != nil {
		return err
	}
	for _, f := range hfs.notifies {
		f(ctx, &FileInfo{fname: fullname, e: e})
	}
	return nil
}

// Stat returns a FileInfo at root/fname.
func (hfs *HashFS) Stat(ctx context.Context, root, fname string) (FileInfo, error) {
	if log.V(1) {
		clog.Infof(ctx, "stat @%s %s", root, fname)
	}
	e, dir, ok := hfs.dirLookup(ctx, root, fname)
	if ok {
		if e.err != nil {
			return FileInfo{}, e.err
		}
		return FileInfo{root: root, fname: fname, e: e}, nil
	}
	fname = filepath.Join(root, fname)
	fname = filepath.ToSlash(fname)
	e = newLocalEntry()
	e.init(ctx, fname, hfs.IOMetrics)
	clog.Infof(ctx, "stat new entry %s %s", fname, e)
	if log.V(9) {
		clog.Infof(ctx, "store %s %s in %s", fname, e, dir)
	}
	var err error
	if dir != nil {
		e, err = dir.store(ctx, filepath.Base(fname), e)
	} else {
		e, err = hfs.directory.store(ctx, fname, e)
	}
	if err != nil {
		clog.Warningf(ctx, "failed to store %s %s in %s: %v", fname, e, dir, err)
		return FileInfo{}, err
	}
	if e.err != nil {
		return FileInfo{}, e.err
	}
	hfs.digester.lazyCompute(ctx, fname, e)
	return FileInfo{root: root, fname: fname, e: e}, nil
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
	e, _, ok := hfs.directory.lookup(ctx, hfs.directory, dname)
	if !ok {
		e = newLocalEntry()
		e.init(ctx, dname, hfs.IOMetrics)
		var err error
		e, err = hfs.directory.store(ctx, dname, e)
		if err != nil {
			clog.Warningf(ctx, "failed to store %s %s: %v", dname, e, err)
			return nil, err
		}
		clog.Infof(ctx, "stat new dir entry %s %s", dname, e)
	}
	err = e.err
	if err != nil {
		return nil, fmt.Errorf("read dir %s: %w", dname, err)
	}
	if e.directory == nil {
		return nil, fmt.Errorf("read dir %s: not dir: %w", dname, os.ErrPermission)
	}
	// TODO(ukai): fix race in updateDir -> store.
	names := e.updateDir(ctx, hfs, dirname)
	if log.V(1) {
		clog.Infof(ctx, "update-dir %s -> %d", dirname, len(names))
	}
	var ents []DirEntry
	e.directory.m.Range(func(k, v any) bool {
		name := k.(string)
		ee := v.(*entry)
		if ee.err != nil {
			return true
		}
		ents = append(ents, DirEntry{
			fi: FileInfo{
				root:  root,
				fname: filepath.ToSlash(filepath.Join(dirname, name)),
				e:     ee,
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
	e, _, ok := hfs.directory.lookup(ctx, hfs.directory, fname)
	if !ok {
		e = newLocalEntry()
		e.init(ctx, fname, hfs.IOMetrics)
		var err error
		e, err = hfs.directory.store(ctx, fname, e)
		if err != nil {
			clog.Warningf(ctx, "failed to store %s %s: %v", fname, e, err)
			return nil, err
		}
		clog.Infof(ctx, "stat new entry %s %s", fname, e)
	}
	err := e.err
	if err != nil {
		return nil, fmt.Errorf("read file %s: %w", fname, err)
	}
	if len(e.buf) > 0 {
		return e.buf, nil
	}
	hfs.digester.compute(ctx, fname, e)
	if e.d.IsZero() {
		return nil, fmt.Errorf("read file %s: no data", fname)
	}
	buf, err := digest.DataToBytes(ctx, digest.NewData(e.src, e.d))
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
	hfs.clean = false
	data := digest.FromBytes(fname, b)
	fname = filepath.Join(root, fname)
	fname = filepath.ToSlash(fname)
	span.SetAttr("fname", fname)
	lready := make(chan bool, 1)
	lready <- true
	mode := fs.FileMode(0644)
	if isExecutable {
		mode |= 0111
	}
	e := &entry{
		lready:  lready,
		size:    data.Digest().SizeBytes,
		mtime:   mtime,
		mode:    mode,
		src:     data,
		d:       data.Digest(),
		buf:     b,
		cmdhash: cmdhash,
	}
	err := hfs.dirStoreAndNotify(ctx, fname, e)
	clog.Infof(ctx, "writefile %s x:%t mtime:%s: %v", fname, isExecutable, mtime, err)
	return err
}

// Symlink creates a symlink to target at root/linkpath with mtime and cmdhash.
func (hfs *HashFS) Symlink(ctx context.Context, root, target, linkpath string, mtime time.Time, cmdhash []byte) error {
	if log.V(1) {
		clog.Infof(ctx, "symlink @%s %s -> %s", root, linkpath, target)
	}
	hfs.clean = false
	linkfname := filepath.Join(root, linkpath)
	linkfname = filepath.ToSlash(linkfname)
	lready := make(chan bool, 1)
	lready <- true
	e := &entry{
		lready:  lready,
		mtime:   mtime,
		mode:    0644 | fs.ModeSymlink,
		cmdhash: cmdhash,
		target:  target,
	}
	err := hfs.dirStoreAndNotify(ctx, linkfname, e)
	clog.Infof(ctx, "symlink @%s %s -> %s: %v", root, linkpath, target, err)
	return err
}

// Copy copies a file from root/src to root/dst with mtime and cmdhash.
// if src is dir, returns error.
func (hfs *HashFS) Copy(ctx context.Context, root, src, dst string, mtime time.Time, cmdhash []byte) error {
	if log.V(1) {
		clog.Infof(ctx, "copy @%s %s to %s", root, src, dst)
	}
	hfs.clean = false
	srcname := filepath.Join(root, src)
	srcfname := filepath.ToSlash(srcname)
	dstfname := filepath.Join(root, dst)
	dstfname = filepath.ToSlash(dstfname)
	e, _, ok := hfs.directory.lookup(ctx, hfs.directory, srcfname)
	if !ok {
		e = newLocalEntry()
		if log.V(9) {
			clog.Infof(ctx, "new entry for copy src %s", srcfname)
		}
		e.init(ctx, srcfname, hfs.IOMetrics)
		var err error
		e, err := hfs.directory.store(ctx, srcfname, e)
		if err != nil {
			clog.Warningf(ctx, "failed to store copy src %s: %v", srcfname, err)
			return err
		}
		clog.Infof(ctx, "copy src new entry %s %s", srcfname, e)
	}
	if err := e.err; err != nil {
		return err
	}
	subdir := e.getDir()
	if subdir != nil {
		return fmt.Errorf("is a directory: %s", srcfname)
	}
	lready := make(chan bool, 1)
	lready <- true
	newEnt := &entry{
		lready:  lready,
		size:    e.size,
		mtime:   mtime,
		mode:    e.mode,
		cmdhash: cmdhash,
		target:  e.target,
		// use the same data source as src if any.
		src: e.src,
		d:   e.d,
		buf: e.buf,
	}
	err := hfs.dirStoreAndNotify(ctx, dstfname, newEnt)
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
	hfs.clean = false
	dirname = filepath.Join(root, dirname)
	dirname = filepath.ToSlash(dirname)
	fi, err := os.Lstat(dirname)
	hfs.IOMetrics.OpsDone(err)
	var mtime time.Time
	if err == nil && fi.IsDir() {
		// already exists
		mtime = fi.ModTime()
	} else {
		err := os.MkdirAll(dirname, 0755)
		hfs.IOMetrics.OpsDone(err)
		if err != nil {
			return err
		}
		fi, err := os.Lstat(dirname)
		if err != nil {
			return err
		}
		mtime = fi.ModTime()
	}
	lready := make(chan bool, 1)
	lready <- true

	e := &entry{
		lready:    lready,
		mtime:     mtime,
		mode:      0644 | fs.ModeDir,
		directory: &directory{},
	}
	err = hfs.dirStoreAndNotify(ctx, dirname, e)
	clog.Infof(ctx, "mkdir %s %s: %v", dirname, mtime, err)
	return err
}

// Remove removes a file at root/fname.
func (hfs *HashFS) Remove(ctx context.Context, root, fname string) error {
	if log.V(1) {
		clog.Infof(ctx, "remove @%s %s", root, fname)
	}
	hfs.clean = false
	fname = filepath.Join(root, fname)
	fname = filepath.ToSlash(fname)
	lready := make(chan bool, 1)
	lready <- true
	e := &entry{
		lready: lready,
		err:    fs.ErrNotExist,
	}
	_, err := hfs.directory.store(ctx, fname, e)
	clog.Infof(ctx, "remove %s: %v", fname, err)
	return err
}

// Forget forgets cached entry for inputs under root.
func (hfs *HashFS) Forget(ctx context.Context, root string, inputs []string) {
	for _, fname := range inputs {
		fullname := filepath.Join(root, fname)
		fullname = filepath.ToSlash(fullname)
		hfs.directory.delete(ctx, fullname)
	}
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
		e, _, ok := hfs.directory.lookup(ctx, hfs.directory, fname)
		if ok {
			if log.V(2) {
				clog.Infof(ctx, "tree cache hit %s", fname)
			}
			ents = append(ents, e)
			if e.mode.IsRegular() {
				e.mu.Lock()
				ready := !e.d.IsZero()
				e.mu.Unlock()
				if !ready {
					wg.Add(1)
					nwait++
					go func() {
						defer wg.Done()
						hfs.digester.compute(ctx, fname, e)
					}()
				}
			}
			continue
		}
		e = newLocalEntry()
		e.init(ctx, fname, hfs.IOMetrics)
		if log.V(1) {
			clog.Infof(ctx, "tree new entry %s", fname)
		}
		e, err := hfs.directory.store(ctx, fname, e)
		if err != nil {
			return nil, err
		}
		ents = append(ents, e)
		wg.Add(1)
		nwait++
		go func() {
			defer wg.Done()
			hfs.digester.compute(ctx, fname, e)
		}()
	}
	// wait ensures all entries have computed the digests.
	_, wspan := trace.NewSpan(ctx, "fs-entries-wait")
	wg.Wait()
	wspan.SetAttr("waits", nwait)
	wspan.Close(nil)
	var entries []merkletree.Entry
	for i, fname := range inputs {
		e := ents[i]
		d := e.digest()
		if e.err != nil || (d.IsZero() && e.target == "" && e.directory == nil) {
			// TODO: hard fail instead?
			clog.Warningf(ctx, "missing %s data:%v target:%q: %v", fname, e.d, e.target, e.err)
			continue
		}
		data := digest.NewData(e.src, d)
		isExecutable := e.mode&0111 != 0
		target := e.target
		if target != "" {
			var tname string
			name := filepath.Join(root, fname)
			elink := e
			for j := 0; j < maxSymlinks; j++ {
				tname = filepath.Join(filepath.Dir(name), elink.target)
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
				elink, _, ok = hfs.directory.lookup(ctx, hfs.directory, name)
				if ok {
					if log.V(2) {
						clog.Infof(ctx, "tree cache hit %s", name)
					}
				} else {
					elink = newLocalEntry()
					elink.init(ctx, name, hfs.IOMetrics)
					if log.V(1) {
						clog.Infof(ctx, "tree new entry %s", name)
					}
					var err error
					elink, err = hfs.directory.store(ctx, name, elink)
					if err != nil {
						return nil, err
					}
					hfs.digester.lazyCompute(ctx, name, elink)
				}
				if elink.err != nil || elink.target == "" {
					break
				}
			}
			if e != elink {
				clog.Infof(ctx, "resolve symlink %s to %s", fname, name)
				target = elink.target
				hfs.digester.compute(ctx, name, elink)
				d := e.digest()
				data = digest.NewData(elink.src, d)
				isExecutable = elink.mode&0111 != 0
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
	hfs.clean = false
	for _, ent := range entries {
		fname := filepath.Join(execRoot, ent.Name)
		fname = filepath.ToSlash(fname)
		switch {
		case !ent.Data.IsZero():
			lready := make(chan bool, 1)
			lready <- true
			mode := fs.FileMode(0644)
			if ent.IsExecutable {
				mode |= 0111
			}
			e := &entry{
				lready:  lready,
				size:    ent.Data.Digest().SizeBytes,
				mtime:   mtime,
				mode:    mode,
				cmdhash: cmdhash,
				action:  action,
				src:     ent.Data,
				d:       ent.Data.Digest(),
			}
			err := hfs.dirStoreAndNotify(ctx, fname, e)
			if err != nil {
				return err
			}
		case ent.Target != "":
			lready := make(chan bool, 1)
			lready <- true
			e := &entry{
				lready:  lready,
				mtime:   mtime,
				mode:    0644 | fs.ModeSymlink,
				cmdhash: cmdhash,
				action:  action,
				target:  ent.Target,
			}
			err := hfs.dirStoreAndNotify(ctx, fname, e)
			if err != nil {
				return err
			}
		default: // directory
			lready := make(chan bool, 1)
			lready <- true
			e := &entry{
				lready:    lready,
				mtime:     mtime,
				mode:      0644 | fs.ModeDir,
				cmdhash:   cmdhash,
				action:    action,
				directory: &directory{},
			}
			err := hfs.dirStoreAndNotify(ctx, fname, e)
			if err != nil {
				return err
			}
			err = os.Chtimes(fname, time.Now(), mtime)
			hfs.IOMetrics.OpsDone(err)
			if err != nil {
				clog.Warningf(ctx, "failed to update dir mtime %s: %v", fname, err)
			}
		}
	}
	return nil
}

// UpdateFromLocal updates cache information for inputs under execRoot with cmdhash from local disk.
// when mtime is zero, it keeps mtime of local file (for restat=true).
// otherwise, it will update mtime.
func (hfs *HashFS) UpdateFromLocal(ctx context.Context, root string, inputs []string, mtime time.Time, cmdhash []byte) error {
	ctx, span := trace.NewSpan(ctx, "fs-update-local")
	defer span.Close(nil)
	hfs.Forget(ctx, root, inputs)
	for _, fname := range inputs {
		fullname := filepath.Join(root, fname)
		fullname = filepath.ToSlash(fullname)
		e := newLocalEntry()
		e.init(ctx, fullname, hfs.IOMetrics)
		e.cmdhash = cmdhash
		clog.Infof(ctx, "stat new entry(local outputs): %s %s", fullname, e)
		err := hfs.dirStoreAndNotify(ctx, fullname, e)
		if err != nil {
			return err
		}
		hfs.digester.compute(ctx, fullname, e)
		if mtime.IsZero() {
			continue
		}
		if !mtime.Equal(e.getMtime()) {
			e.mu.Lock()
			e.mtime = mtime
			e.mu.Unlock()
			err = os.Chtimes(fullname, time.Now(), mtime)
			hfs.IOMetrics.OpsDone(err)
			if errors.Is(err, fs.ErrNotExist) {
				clog.Warningf(ctx, "failed to update mtime of %s: %v", fullname, err)
				continue
			}
			if err != nil {
				return fmt.Errorf("failed to update mtime of %s: %w", fullname, err)
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

func (noDataSource) Source(d digest.Digest, fname string) digest.Source {
	return noSource{fname}
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
		e, _, ok := hfs.directory.lookup(ctx, hfs.directory, fname)
		if !ok {
			// If it doesn't exist in memory, just use local disk as is.
			continue
		}
		select {
		case need := <-e.lready:
			if !need {
				if log.V(1) {
					clog.Infof(ctx, "flush %s local ready", fname)
				}
				err := e.err
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

		hfs.digester.compute(ctx, fname, e)
		ctx, done, err := FlushSemaphore.WaitAcquire(ctx)
		if err != nil {
			return fmt.Errorf("flush %s: %w", fname, err)
		}
		eg.Go(func() error {
			defer done()
			return e.flush(ctx, fname, hfs.IOMetrics)
		})
	}
	return eg.Wait()
}

// Refresh refreshes cached file entries under execRoot.
func (hfs *HashFS) Refresh(ctx context.Context, execRoot string) error {
	// TODO: optimize?
	state := hfs.State(ctx)
	return hfs.SetState(ctx, state)
}

type entry struct {
	// lready represents whether it is ready to use local file.
	// true - need to download contents.
	// block - download is in progress.
	// closed/false - file is already downloaded.
	lready chan bool

	err  error
	size int64
	mode fs.FileMode

	// cmdhash is hash of command lines that generated this file.
	// e.g. hash('touch output.stamp')
	cmdhash []byte

	// digest of action that generated this file.
	action digest.Digest

	target string // symlink.

	src digest.Source
	buf []byte // from WriteFile.

	mu sync.RWMutex
	// mtime of entry in hashfs.
	mtime     time.Time
	d         digest.Digest
	directory *directory
}

func newLocalEntry() *entry {
	lready := make(chan bool)
	close(lready)
	return &entry{
		lready: lready,
	}
}

func (e *entry) String() string {
	return fmt.Sprintf("size:%d mode:%s mtime:%s", e.size, e.mode, e.getMtime())
}

func (e *entry) init(ctx context.Context, fname string, m *iometrics.IOMetrics) {
	fi, err := os.Lstat(fname)
	m.OpsDone(err)
	if errors.Is(err, fs.ErrNotExist) {
		if log.V(1) {
			clog.Infof(ctx, "not exist %s", fname)
		}
		e.err = err
		return
	}
	if err != nil {
		clog.Warningf(ctx, "failed to lstat %s: %v", fname, err)
		e.err = err
		return
	}
	switch {
	case fi.IsDir():
		if log.V(1) {
			clog.Infof(ctx, "tree entry %s: is dir", fname)
		}
		e.directory = &directory{}
		e.mode = 0644 | fs.ModeDir
	case fi.Mode().Type() == fs.ModeSymlink:
		e.mode = 0644 | fs.ModeSymlink
		e.target, err = os.Readlink(fname)
		m.OpsDone(err)
		if err != nil {
			e.err = err
		}
		if log.V(1) {
			clog.Infof(ctx, "tree entry %s: symlink to %s: %v", fname, e.target, e.err)
		}
	case fi.Mode().IsRegular():
		e.mode = 0644
		if isExecutable(fi, fname) {
			e.mode |= 0111
		}
		e.src = digest.LocalFileSource{Fname: fname, IOMetrics: m}
	default:
		e.err = fmt.Errorf("unexpected filetype not regular %s: %s", fi.Mode(), fname)
		clog.Errorf(ctx, "tree entry %s: unknown filetype %s", fname, fi.Mode())
		return
	}
	if e.mtime.Before(fi.ModTime()) {
		e.mtime = fi.ModTime()
	}
}

func (e *entry) compute(ctx context.Context, fname string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.err != nil {
		return e.err
	}
	if !e.d.IsZero() {
		return nil
	}
	// TODO(b/271059955): add xattr support.
	src, ok := e.src.(digest.LocalFileSource)
	if !ok {
		return nil
	}
	data, err := digest.FromLocalFile(ctx, src)
	if err != nil {
		return err
	}
	e.d = data.Digest()
	return nil
}

func (e *entry) digest() digest.Digest {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.d
}

func (e *entry) getMtime() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.mtime
}

func (e *entry) updateDir(ctx context.Context, hfs *HashFS, dname string) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	d, err := os.Open(dname)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			clog.Warningf(ctx, "updateDir %s: open %v", dname, err)
		}
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
	if fi.ModTime().Equal(e.directory.mtime) {
		if log.V(1) {
			clog.Infof(ctx, "updateDir %s: up-to-date %s", dname, e.mtime)
		}
		return nil
	}
	names, err := d.Readdirnames(-1)
	if err != nil {
		clog.Warningf(ctx, "updateDir %s: readdirnames %v", dname, err)
		return nil
	}
	for _, name := range names {
		// update entry in e.directory.
		_, err = hfs.Stat(ctx, dname, name)
		if err != nil {
			clog.Warningf(ctx, "updateDir stat %s: %v", name, err)
		}
	}
	clog.Infof(ctx, "updateDir mtime %s %d %s -> %s", dname, len(names), e.mtime, fi.ModTime())
	e.directory.mtime = fi.ModTime()
	// if local dir is updated after hashfs update, update hashfs mtime.
	if e.mtime.Before(e.directory.mtime) {
		e.mtime = e.directory.mtime
	}
	return names
}

// getDir returns directory of entry.
func (e *entry) getDir() *directory {
	if e == nil {
		return nil
	}
	return e.directory
}

func (e *entry) flush(ctx context.Context, fname string, m *iometrics.IOMetrics) error {
	defer close(e.lready)

	if errors.Is(e.err, fs.ErrNotExist) {
		err := os.RemoveAll(fname)
		m.OpsDone(err)
		clog.Infof(ctx, "flush remove %s: %v", fname, err)
		return nil
	}
	d := e.digest()
	mtime := e.getMtime()
	switch {
	case e.directory != nil:
		// directory
		fi, err := os.Lstat(fname)
		m.OpsDone(err)
		if err == nil && fi.IsDir() && fi.ModTime().Equal(mtime) {
			clog.Infof(ctx, "flush dir %s: already exist", fname)
			return nil
		}
		err = os.MkdirAll(fname, 0755)
		m.OpsDone(err)
		clog.Infof(ctx, "flush dir %s: %v", fname, err)
		return err
	case d.IsZero() && e.target != "":
		err := os.Symlink(e.target, fname)
		m.OpsDone(err)
		if errors.Is(err, fs.ErrExist) {
			err = os.Remove(fname)
			m.OpsDone(err)
			err = os.Symlink(e.target, fname)
			m.OpsDone(err)
		}
		clog.Infof(ctx, "flush symlink %s -> %s: %v", fname, e.target, err)
		// don't change mtimes. it fails if target doesn't exist.
		return err
	default:
	}
	fi, err := os.Lstat(fname)
	m.OpsDone(err)
	if err == nil {
		if fi.Size() == d.SizeBytes && fi.ModTime().Equal(mtime) {
			// TODO: check hash, mode?
			clog.Infof(ctx, "flush %s: already exist", fname)
			return nil
		}
		if isHardlink(fi) {
			clog.Infof(ctx, "flush %s: remove hardlink", fname)
			err = os.Remove(fname)
			m.OpsDone(err)
		} else {
			var fileDigest digest.Digest
			ld, err := localDigest(ctx, fname, m)
			if err == nil {
				fileDigest = ld.Digest()
				if fileDigest == d {
					clog.Infof(ctx, "flush %s: already exist - hash match", fname)
					if !fi.ModTime().Equal(mtime) {
						err = os.Chtimes(fname, time.Now(), mtime)
						m.OpsDone(err)
					}
					return err
				}
			}
			clog.Warningf(ctx, "flush %s: exists but mismatch size:%d!=%d mtime:%s!=%s d:%v!=%v", fname, fi.Size(), d.SizeBytes, fi.ModTime(), mtime, fileDigest, d)
			if fi.Mode()&0200 == 0 {
				// need to be writable. otherwise os.WriteFile fails with permission denied.
				err = os.Chmod(fname, fi.Mode()|0200)
				m.OpsDone(err)
				clog.Warningf(ctx, "flush %s: not writable? %s: %v", fname, fi.Mode(), err)
			}
		}
	}
	err = os.MkdirAll(filepath.Dir(fname), 0755)
	m.OpsDone(err)
	if err != nil {
		clog.Warningf(ctx, "flush %s: mkdir: %v", fname, err)
		return fmt.Errorf("failed to create directory for %s: %w", fname, err)
	}
	if d.SizeBytes == 0 {
		clog.Infof(ctx, "flush %s: empty file", fname)
		err := os.WriteFile(fname, nil, 0644)
		m.WriteDone(0, err)
		if err != nil {
			return err
		}
		err = os.Chtimes(fname, time.Now(), mtime)
		m.OpsDone(err)
		if err != nil {
			return err
		}
		return nil
	}
	buf := e.buf
	if len(buf) == 0 {
		if e.d.IsZero() {
			return fmt.Errorf("no data: retrieve %s: ", fname)
		}
		buf, err = digest.DataToBytes(ctx, digest.NewData(e.src, d))
		clog.Infof(ctx, "flush %s %s from source: %v", fname, d, err)
		if err != nil {
			return fmt.Errorf("flush %s size=%d: %w", fname, d.SizeBytes, err)
		}
	} else {
		clog.Infof(ctx, "flush %s from embedded buf", fname)
	}
	err = os.WriteFile(fname, buf, e.mode)
	m.WriteDone(len(buf), err)
	if err != nil {
		return err
	}
	err = os.Chtimes(fname, time.Now(), mtime)
	m.OpsDone(err)
	if err != nil {
		return err
	}
	return nil
}

// directory is per-directory entry map to reduce mutex contention.
// TODO: use generics as DirMap<K,V>?
type directory struct {
	// mtime on the local disk when it reads the dir.
	mtime time.Time
	// m is a map of file in a directory's basename to *entry.
	m sync.Map
}

func (d *directory) String() string {
	if d == nil {
		return "<nil>"
	}
	// better to dump all entries?
	return fmt.Sprintf("&directory{m:%p}", &d.m)
}

func (d *directory) lookup(ctx context.Context, root *directory, fname string) (*entry, *directory, bool) {
	origFname := fname
	n := 0
	for fname != "" {
		n++
		fname = strings.TrimPrefix(fname, "/")
		elem, rest, ok := strings.Cut(fname, "/")
		if !ok {
			e, ok := d.m.Load(fname)
			if !ok {
				return nil, d, ok
			}
			return e.(*entry), d, ok
		}
		fname = rest
		v, ok := d.m.Load(elem)
		var subdir *directory
		if ok {
			e := v.(*entry)
			if e != nil && e.target != "" {
				// Resolve a symlink and lookup the target from root.
				target := resolveSymlink(ctx, origFname, n, e.target)
				// TODO(289869742): optimize for e.target is basename only?
				// TODO(289869742): return resolved path and traverse again in caller side?
				e, _, ok = root.lookup(ctx, root, target)
				if !ok {
					return nil, nil, false
				}
				// resolve symlink until dir or reach the limit?
			}
			subdir = e.getDir()
		}
		if subdir == nil {
			if log.V(1) {
				lv := struct {
					origFname, elem, fname string
					subdir                 *directory
				}{origFname, elem, fname, subdir}
				clog.Infof(ctx, "lookup %s subdir %s %s -> %s", lv.origFname, lv.elem, lv.fname, lv.subdir)
			}
			return nil, nil, false
		}
		d = subdir
	}
	if log.V(1) {
		logOrigFname := origFname
		clog.Infof(ctx, "lookup %s fname empty", logOrigFname)
	}
	return nil, nil, false
}

func resolveSymlink(ctx context.Context, origFname string, n int, target string) string {
	// reconstruct elem's list as we traversed.
	// we do this again to avoid unnecessary allocation for normal case.
	elems := make([]string, 0, n)
	if strings.HasPrefix(origFname, "/") {
		elems = append(elems, "/")
	}
	s := origFname
	for i := 0; i < n-1; i++ {
		s = strings.TrimPrefix(s, "/")
		elem, rest, _ := strings.Cut(s, "/")
		elems = append(elems, elem)
		s = rest
	}
	elems = append(elems, target)
	return filepath.Join(elems...)
}

func (d *directory) store(ctx context.Context, fname string, e *entry) (*entry, error) {
	origFname := fname
	elems := make([]string, 0, strings.Count(fname, "/")+1)
	if log.V(8) {
		logOrigFname := origFname
		clog.Infof(ctx, "store %s %v", logOrigFname, e)
	}
	if strings.HasPrefix(fname, "/") {
		elems = append(elems, "/")
	}
	for fname != "" {
		fname = strings.TrimPrefix(fname, "/")
		elem, rest, ok := strings.Cut(fname, "/")
		if !ok {
			v, loaded := d.m.LoadOrStore(fname, e)
			if !loaded {
				if log.V(8) {
					lv := struct {
						origFname string
						d         *directory
						fname     string
					}{origFname, d, fname}
					clog.Infof(ctx, "store %s -> %p %s", lv.origFname, lv.d, lv.fname)
				}
				return e, nil
			}
			// check whether there is an update from previous entry.
			ee := v.(*entry)
			eed := ee.digest()
			cmdchanged := !bytes.Equal(ee.cmdhash, e.cmdhash)
			actionchanged := ee.action != e.action
			if e.target != "" && ee.target != e.target {
				lv := struct {
					origFname         string
					cmdchanged        bool
					eetarget, etarget string
				}{origFname, cmdchanged, ee.target, e.target}
				clog.Infof(ctx, "store %s: cmdchange:%t s:%q to %q", lv.origFname, lv.cmdchanged, lv.eetarget, lv.etarget)
			} else if !e.d.IsZero() && eed != e.d && eed.SizeBytes != 0 && e.d.SizeBytes != 0 {
				// don't log nil to digest of empty file (size=0)
				lv := struct {
					origFname  string
					cmdchanged bool
					eed, ed    digest.Digest
				}{origFname, cmdchanged, eed, e.d}
				clog.Infof(ctx, "store %s: cmdchange:%t d:%v to %v", lv.origFname, lv.cmdchanged, lv.eed, lv.ed)
			} else if cmdchanged || actionchanged {
				lv := struct {
					origFname     string
					cmdchanged    bool
					actionchanged bool
				}{origFname, cmdchanged, actionchanged}
				clog.Infof(ctx, "store %s: cmdchange:%t actionchange:%t", lv.origFname, lv.cmdchanged, lv.actionchanged)
			} else if ee.target == e.target && ee.size == e.size && ee.mode == e.mode && (e.d.IsZero() || eed == e.d) {
				// no change?

				// if e.d is zero, it may be new local entry
				// and ee.d has been calculated

				// update mtime.
				ee.mu.Lock()
				ee.mtime = e.mtime
				ee.mu.Unlock()
				return ee, nil
			}
			// e should be new value for fname.
			swapped := d.m.CompareAndSwap(fname, ee, e)
			if !swapped {
				// store race?
				v, ok := d.m.Load(fname)
				return nil, fmt.Errorf("store race %s: %p -> %p -> %p %t", fname, ee, e, v, ok)
			}
			// e is stored for fname
			return e, nil

		}
		elems = append(elems, elem)
		fname = rest
		i := 0
		for ; i < maxSymlinks; i++ {
			nextDir, target, err := d.nextDir(ctx, elems, origFname, fname)
			if err != nil {
				return nil, err
			}
			if nextDir != nil {
				d = nextDir
				break
			}
			if target != "" {
				if !strings.Contains(target, "/") {
					elems[len(elems)-1] = target
					continue
				}
				// TODO(b/289869742): handle symlink target is not just basename.
				d = nil
				break
			}
		}
		if d == nil || i == maxSymlinks {
			return nil, fmt.Errorf("failed to set entry: %s", strings.Join(elems, "/"))
		}
	}
	errOrigFname := origFname
	return nil, fmt.Errorf("bad fname? %q", errOrigFname)
}

func (d *directory) nextDir(ctx context.Context, elems []string, origFname, fname string) (*directory, string, error) {
	elem := elems[len(elems)-1]
	v, ok := d.m.Load(elem)
	if ok {
		dent := v.(*entry)
		if dent != nil && dent.err == nil {
			target := dent.target
			subdir := dent.getDir()
			if log.V(9) {
				lv := struct {
					origFname, elem, fname string
					d                      *directory
					dent                   *entry
				}{origFname, elem, fname, d, dent}
				clog.Infof(ctx, "store %s subdir0 %s %s -> %s (%v)", lv.origFname, lv.elem, lv.fname, lv.d, lv.dent)
			}
			if subdir == nil && target == "" {
				ev := struct {
					elem string
					dent *entry
				}{elem, dent}
				return nil, "", fmt.Errorf("failed to set entry: %s not dir %#v", ev.elem, ev.dent)
			}
			d = subdir
			return d, target, nil
		}
		deleted := d.m.CompareAndDelete(elem, dent)
		if log.V(9) {
			lv := struct {
				origFname, elem string
				deleted         bool
			}{origFname, elem, deleted}
			clog.Infof(ctx, "store %s delete missing %s to create dir deleted: %t", lv.origFname, lv.elem, lv.deleted)
		}
	}
	// create intermediate dir of elem.
	mtime := time.Now()
	dfi, err := os.Lstat(filepath.Join(elems...))
	if err == nil {
		mtime = dfi.ModTime()
		switch {
		case dfi.IsDir():
		case dfi.Mode().Type() == fs.ModeSymlink:
			target, err := os.Readlink(filepath.Join(elems...))
			if err != nil {
				return nil, "", fmt.Errorf("failed to set entry %s symlink: %w", filepath.Join(elems...), err)
			}
			return nil, target, nil
		default:
			return nil, "", fmt.Errorf("failed to set entry: %s not dir", filepath.Join(elems...))
		}
	}
	lready := make(chan bool, 1)
	lready <- true
	newDent := &entry{
		lready: lready,
		mode:   0644 | fs.ModeDir,
		mtime:  mtime,
		// don't set directory.mtime for intermediate dir.
		// mtime will be updated by updateDir
		// when all dirents have been loaded.
		directory: &directory{},
	}
	dent := newDent
	v, ok = d.m.LoadOrStore(elem, dent)
	if ok {
		dent = v.(*entry)
	}
	var target string
	if dent != nil {
		target = dent.target
	}
	subdir := dent.getDir()
	if log.V(9) {
		lv := struct {
			origFname, elem, fname string
			subdir                 *directory
			dent                   *entry
		}{origFname, elem, fname, subdir, dent}
		clog.Infof(ctx, "store %s subdir1 %s %s -> %s (%v)", lv.origFname, lv.elem, lv.fname, lv.subdir, lv.dent)
	}
	d = subdir
	if d == nil && target == "" {
		ev := struct {
			elem string
			dent *entry
		}{elem, dent}
		return nil, target, fmt.Errorf("failed to set entry: %s not dir %#v", ev.elem, ev.dent)
	}
	return d, target, nil
}

func (d *directory) delete(ctx context.Context, fname string) {
	for fname != "" {
		fname = strings.TrimPrefix(fname, "/")
		elem, rest, ok := strings.Cut(fname, "/")
		if !ok {
			d.m.Delete(fname)
			return
		}
		fname = rest
		v, ok := d.m.Load(elem)
		if !ok {
			return
		}
		e := v.(*entry)
		d = e.getDir()
		if d == nil {
			return
		}
	}
}

// FileInfo implements https://pkg.go.dev/io/fs#FileInfo.
type FileInfo struct {
	root  string
	fname string
	e     *entry
}

func (fi *FileInfo) Path() string {
	return filepath.ToSlash(filepath.Join(fi.root, fi.fname))
}

// Name is a base name of the file.
func (fi FileInfo) Name() string {
	return filepath.Base(fi.fname)
}

// Size is a size of the file.
func (fi FileInfo) Size() int64 {
	return fi.e.size
}

// Mode is a file mode of the file.
func (fi FileInfo) Mode() fs.FileMode {
	return fi.e.mode
}

// ModTime is a modification time of the file.
func (fi FileInfo) ModTime() time.Time {
	return fi.e.getMtime()
}

// IsDir returns true if it is the directory.
func (fi FileInfo) IsDir() bool {
	return fi.e.mode.IsDir()
}

// Sys returns merkletree Entry of the file.
// For local file, digest may not be calculated yet.
// Use Entries to get correct merkletree.Entry.
func (fi FileInfo) Sys() any {
	d := fi.e.digest()
	return merkletree.Entry{
		Name:         fi.Path(),
		Data:         digest.NewData(fi.e.src, d),
		IsExecutable: fi.e.mode&0111 != 0,
		Target:       fi.e.target,
	}
}

// CmdHash returns a cmdhash that created the file.
func (fi FileInfo) CmdHash() []byte {
	return fi.e.cmdhash
}

// Action returns a digest of action that created the file.
func (fi FileInfo) Action() digest.Digest {
	return fi.e.action
}

// Target returns a symlink target of the file, or empty if it is not symlink.
func (fi FileInfo) Target() string {
	return fi.e.target
}

// DirEntry implements https://pkg.go.dev/io/fs#DirEntry.
type DirEntry struct {
	fi FileInfo
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
