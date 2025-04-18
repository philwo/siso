// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package hashfs provides a filesystem with digest hash.
package hashfs

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/charmbracelet/log"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"go.chromium.org/infra/build/siso/hashfs/osfs"
	pb "go.chromium.org/infra/build/siso/hashfs/proto"
	"go.chromium.org/infra/build/siso/reapi/digest"
	"go.chromium.org/infra/build/siso/reapi/merkletree"
)

// Linux imposes a limit of at most 40 symlinks in any one path lookup.
// see: https://lwn.net/Articles/650786/
const maxSymlinks = 40

func isExecutable(fi fs.FileInfo, fname string, m map[string]bool) bool {
	if fi.Mode()&0111 != 0 {
		return true
	}
	return m[fname]
}

// NotifyFunc is the type of the function to notify the filesystem changes.
type NotifyFunc func(context.Context, *FileInfo)

// HashFS is a filesystem for digest hash.
type HashFS struct {
	opt       Option
	directory *directory

	notifies []NotifyFunc

	// loadErr keeps load error.
	loadErr error

	// clean if loaded state is matched with local disk's state.
	clean atomic.Bool

	// buildTargets is build targets stored in state file.
	// nil vs []string{} differs.
	// nil is not set (last build failed).
	// []string{} is set (last build succeeded without explicit target requested).
	buildTargets []string

	// loaded if state is loaded.
	loaded atomic.Bool

	// holds generated files (full path) in previous builds.
	previouslyGeneratedFiles []string

	executables map[string]bool

	journalMu sync.Mutex
	// writer for updated entries journal.
	journal io.WriteCloser

	// trigger for SetState background goroutine finish.
	setStateCh chan error
}

// New creates a HashFS.
func New(ctx context.Context, opt Option) (*HashFS, error) {
	if opt.OutputLocal == nil {
		opt.OutputLocal = func(context.Context, string) bool { return false }
	}
	if opt.Ignore == nil {
		opt.Ignore = func(context.Context, string) bool { return false }
	}
	if opt.DataSource == nil {
		opt.DataSource = noDataSource{}
	}
	fsys := &HashFS{
		opt:       opt,
		directory: &directory{isRoot: true},
	}
	if opt.StateFile != "" {
		start := time.Now()
		journalFile := opt.StateFile + ".journal"

		fstate, err := Load(opt)
		if errors.Is(err, fs.ErrNotExist) {
			log.Infof("missing fs state. new build? %v", err)
			// missing .siso_fs_state is not considered as load error.
			fstate = &pb.State{}
		} else if err != nil {
			log.Warnf("Failed to load fs state from %s: %v", opt.StateFile, err)
			fsys.loadErr = err
			fstate = &pb.State{}
		} else {
			log.Infof("Load fs state from %s: %s", opt.StateFile, time.Since(start))
		}

		// for corrupted fs state, we also don't use journal,
		// as we don't have base state for the journal.
		if fsys.loadErr == nil {
			// if previous build didn't finish properly, journal file
			// is not removed, so recover last build updates from the journal.
			reconciled := loadJournal(journalFile, fstate)
			if err := fsys.SetState(ctx, fstate); err != nil {
				return nil, err
			}
			if reconciled {
				// save fstate to make it base state for next journaling.
				err := Save(fstate, opt)
				if err != nil {
					log.Errorf("Failed to save reconciled fs state in %s: %v", opt.StateFile, err)
				}
			}
		}
		err = os.Remove(journalFile)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			log.Warnf("Failed to remove journal: %v", err)
		}

		f, err := os.Create(journalFile)
		if err != nil {
			log.Warnf("Failed to create fs state journal: %v", err)
		} else {
			fsys.journal = f
		}
	}
	return fsys, nil
}

// LoadErr returns load error.
func (hfs *HashFS) LoadErr() error {
	return hfs.loadErr
}

// WaitReady waits fs state is updated in memory.
func (hfs *HashFS) WaitReady(ctx context.Context) error {
	if hfs.setStateCh == nil {
		return nil
	}
	started := time.Now()
	select {
	case <-ctx.Done():
		log.Errorf("hashfs does not become ready %s: %v", time.Since(started), context.Cause(ctx))
		return context.Cause(ctx)

	case err := <-hfs.setStateCh:
		hfs.setStateCh = nil
		if err != nil {
			log.Errorf("hashfs does not become ready %s: %v", time.Since(started), err)
			return err
		}
		log.Infof("hashfs becomes ready: %v", time.Since(started))
	}
	return nil
}

// Notify causes the hashfs to relay filesystem motification to f.
func (hfs *HashFS) Notify(f NotifyFunc) {
	hfs.notifies = append(hfs.notifies, f)
}

// SetExecutables sets a map of full paths for files to be
// considered as executable, even if it is not executable on local disk.
func (hfs *HashFS) SetExecutables(m map[string]bool) {
	hfs.executables = m
}

// SetBuildTargets sets build targets.
func (hfs *HashFS) SetBuildTargets(buildTargets []string, success bool) {
	if !success {
		hfs.buildTargets = nil
		return
	}
	hfs.buildTargets = make([]string, len(buildTargets))
	copy(hfs.buildTargets, buildTargets)
}

// Close closes the HashFS.
// Persists current state in opt.StateFile.
func (hfs *HashFS) Close(ctx context.Context) error {
	if hfs.opt.StateFile == "" {
		return nil
	}
	hfs.journalMu.Lock()
	if hfs.journal != nil {
		err := hfs.journal.Close()
		if err != nil {
			log.Warnf("Failed to close journal %v", err)
		}
		hfs.journal = nil
	}
	hfs.journalMu.Unlock()
	if hfs.clean.Load() || !hfs.loaded.Load() {
		log.Warnf("not save state clean=%t loaded=%t", hfs.clean.Load(), hfs.loaded.Load())
		return nil
	}
	err := Save(hfs.State(ctx), hfs.opt)
	if err != nil {
		log.Errorf("Failed to save fs state in %s: %v", hfs.opt.StateFile, err)
		if rerr := os.Remove(hfs.opt.StateFile); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
			log.Errorf("Failed to remove stale fs state %s: %v", hfs.opt.StateFile, err)
		}
		return err
	}
	log.Infof("Saved fs state in %s", hfs.opt.StateFile)
	return nil
}

// IsClean returns whether hashfs is clean for buildTargets (i.e. sync with local disk).
func (hfs *HashFS) IsClean(buildTargets []string) bool {
	if !hfs.clean.Load() {
		return false
	}
	// we distinguish nil vs []string{}.
	if hfs.buildTargets == nil {
		return false
	}
	return slices.Equal(hfs.buildTargets, buildTargets)
}

// PreviouslyGeneratedFiles returns a list of generated files
// (i.e. has cmdhash) in the previous builds.
// It will reset internal data, so next call will return nil
func (hfs *HashFS) PreviouslyGeneratedFiles() []string {
	p := hfs.previouslyGeneratedFiles
	hfs.previouslyGeneratedFiles = nil
	return p
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
	if filepath.IsAbs(fname) {
		return hfs.directory.lookup(ctx, filepath.ToSlash(fname))
	}
	if needPathClean(root, fname) {
		return hfs.directory.lookup(ctx, filepath.ToSlash(filepath.Join(root, fname)))
	}
	e, _, ok := hfs.directory.lookup(ctx, root)
	if !ok {
		return nil, nil, false
	}
	if e.directory == nil {
		return nil, nil, false
	}
	e, dir, resolved, ok := e.directory.lookupEntry(ctx, fname)
	if ok {
		return e, dir, true
	}
	if resolved != "" {
		resolvedName := resolved
		if !filepath.IsAbs(resolved) {
			resolvedName = filepath.ToSlash(filepath.Join(root, resolved))
		}
		return hfs.directory.lookup(ctx, resolvedName)
	}
	return nil, nil, false
}

func (hfs *HashFS) dirStoreAndNotify(ctx context.Context, fullname string, e *entry) error {
	ee, err := hfs.directory.store(ctx, fullname, e)
	if err != nil {
		return err
	}
	go ee.compute(ctx)
	for _, f := range hfs.notifies {
		f(ctx, &FileInfo{fname: fullname, e: ee})
	}
	return nil
}

// Stat returns a FileInfo at root/fname.
func (hfs *HashFS) Stat(ctx context.Context, root, fname string) (FileInfo, error) {
	e, dir, ok := hfs.dirLookup(ctx, root, fname)
	if ok {
		if e.err != nil {
			return FileInfo{}, e.err
		}
		if e.directory != nil {
			// directory's mtime has been updated locally
			// where hashfs doesn't know. e.g. add new file
			// in the directory by local run.
			fullname := filepath.Join(root, fname)
			lfi, err := os.Lstat(fullname)
			switch {
			case errors.Is(err, fs.ErrNotExist):
				// virtually created dir in hashfs,
				// so no need to update mtime.
			case err != nil:
				log.Warnf("unexpected dir stat fail %s: %v", fullname, err)
				return FileInfo{}, err
			default:
				mtime := lfi.ModTime()
				if now := time.Now(); mtime.After(now) {
					log.Warnf("future timestamp on %s: mtime=%s now=%s", fullname, mtime, now)
					return FileInfo{}, fmt.Errorf("future timestamp on %s: mtime=%s now=%s", fullname, mtime, now)
				}
				e.mu.Lock()
				e.mtime = mtime
				if e.updatedTime.Before(mtime) {
					// if no cmdhash, it may not be generated by any step, so keep updated_time with mtime silently.
					if len(e.cmdhash) > 0 {
						log.Warnf("unexpected update dir mtime %s %v; updated_time=%v", fullname, mtime, e.updatedTime)
					}
					e.updatedTime = mtime
				}
				e.mu.Unlock()
			}
		}
		return FileInfo{root: root, fname: fname, e: e}, nil
	}
	if !filepath.IsAbs(fname) {
		fname = filepath.Join(root, fname)
	}
	fname = filepath.ToSlash(fname)
	e = newLocalEntry()
	e.init(fname, hfs.executables)
	if errors.Is(e.err, context.Canceled) {
		return FileInfo{}, e.err
	}
	var err error
	if dir != nil {
		e, err = dir.store(ctx, filepath.Base(fname), e)
		if errors.Is(err, errRootSymlink) {
			e, err = hfs.directory.store(ctx, fname, e)
		}
	} else {
		e, err = hfs.directory.store(ctx, fname, e)
	}
	if err != nil {
		log.Warnf("failed to store %s %s in %s: %v", fname, e, dir, err)
		return FileInfo{}, err
	}
	if e.err != nil {
		return FileInfo{}, e.err
	}
	go e.compute(ctx)
	return FileInfo{root: root, fname: fname, e: e}, nil
}

// ReadDir returns directory entries of root/name.
func (hfs *HashFS) ReadDir(ctx context.Context, root, name string) (dents []DirEntry, err error) {
	dirname := name
	if !filepath.IsAbs(name) {
		dirname = filepath.Join(root, name)
	}
	dname := filepath.ToSlash(dirname)
	e, _, ok := hfs.directory.lookup(ctx, dname)
	if !ok {
		e = newLocalEntry()
		e.init(dname, hfs.executables)
		if errors.Is(e.err, context.Canceled) {
			return nil, e.err
		}
		var err error
		e, err = hfs.directory.store(ctx, dname, e)
		if err != nil {
			log.Warnf("failed to store %s %s: %v", dname, e, err)
			return nil, err
		}
	}
	err = e.err
	if err != nil {
		return nil, fmt.Errorf("read dir %s: %w", dname, err)
	}
	if e.directory == nil {
		return nil, fmt.Errorf("read dir %s: not dir: %w", dname, os.ErrPermission)
	}
	// TODO(ukai): fix race in updateDir -> store.
	_ = e.updateDir(ctx, hfs, dirname)
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
	fname = filepath.Join(root, fname)
	fname = filepath.ToSlash(fname)
	e, _, ok := hfs.directory.lookup(ctx, fname)
	if !ok {
		e = newLocalEntry()
		e.init(fname, hfs.executables)
		if errors.Is(e.err, context.Canceled) {
			return nil, e.err
		}
		var err error
		e, err = hfs.directory.store(ctx, fname, e)
		if err != nil {
			log.Warnf("failed to store %s %s: %v", fname, e, err)
			return nil, err
		}
	}
	err := e.err
	if err != nil {
		return nil, fmt.Errorf("read file %s: %w", fname, err)
	}
	if len(e.buf) > 0 {
		return e.buf, nil
	}
	e.compute(ctx)
	if e.d.IsZero() {
		return nil, fmt.Errorf("read file %s: no data", fname)
	}
	buf, err := digest.DataToBytes(ctx, digest.NewData(e.src, e.d))
	return buf, err
}

// WriteFile writes a contents in root/fname with mtime and cmdhash.
func (hfs *HashFS) WriteFile(ctx context.Context, root, fname string, b []byte, isExecutable bool, mtime time.Time, cmdhash []byte) error {
	hfs.clean.Store(false)
	data := digest.FromBytes(fname, b)
	fname = filepath.Join(root, fname)
	fname = filepath.ToSlash(fname)
	lready := make(chan bool, 1)
	lready <- true
	mode := fs.FileMode(0644)
	if isExecutable {
		mode |= 0111
	}
	e := &entry{
		lready:      lready,
		size:        data.Digest().SizeBytes,
		mtime:       mtime,
		mode:        mode,
		src:         data,
		d:           data.Digest(),
		buf:         b,
		cmdhash:     cmdhash,
		updatedTime: time.Now(),
		isChanged:   true,
	}
	err := hfs.dirStoreAndNotify(ctx, fname, e)
	if err != nil {
		return err
	}
	hfs.journalEntry(ctx, fname, e)
	return nil
}

// Symlink creates a symlink to target at root/linkpath with mtime and cmdhash.
func (hfs *HashFS) Symlink(ctx context.Context, root, target, linkpath string, mtime time.Time, cmdhash []byte) error {
	hfs.clean.Store(false)
	linkfname := filepath.Join(root, linkpath)
	linkfname = filepath.ToSlash(linkfname)
	lready := make(chan bool, 1)
	lready <- true
	e := &entry{
		lready:      lready,
		mtime:       mtime,
		mode:        0644 | fs.ModeSymlink,
		cmdhash:     cmdhash,
		target:      target,
		updatedTime: time.Now(),
		isChanged:   true,
	}
	err := hfs.dirStoreAndNotify(ctx, linkfname, e)
	if err != nil {
		return err
	}
	hfs.journalEntry(ctx, linkfname, e)
	return nil
}

// Copy copies a file from root/src to root/dst with mtime and cmdhash.
// if src is dir, returns error.
func (hfs *HashFS) Copy(ctx context.Context, root, src, dst string, mtime time.Time, cmdhash []byte) error {
	hfs.clean.Store(false)
	srcname := src
	if !filepath.IsAbs(src) {
		srcname = filepath.Join(root, src)
	}
	srcfname := filepath.ToSlash(srcname)
	dstfname := filepath.Join(root, dst)
	dstfname = filepath.ToSlash(dstfname)
	e, _, ok := hfs.directory.lookup(ctx, srcfname)
	if !ok {
		e = newLocalEntry()
		e.init(srcfname, hfs.executables)
		if errors.Is(e.err, context.Canceled) {
			return e.err
		}
		var err error
		_, err = hfs.directory.store(ctx, srcfname, e)
		if err != nil {
			log.Warnf("failed to store copy src %s: %v", srcfname, err)
			return err
		}
	}
	if err := e.err; err != nil {
		return err
	}
	subdir := e.getDir()
	if subdir != nil {
		return fmt.Errorf("is a directory: %s", srcfname)
	}
	if e.target == "" {
		e.compute(ctx)
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
		src:         e.src,
		d:           e.d,
		buf:         e.buf,
		updatedTime: time.Now(),
		isChanged:   true,
	}
	err := hfs.dirStoreAndNotify(ctx, dstfname, newEnt)
	if err != nil {
		return err
	}
	hfs.journalEntry(ctx, dstfname, newEnt)
	return nil
}

// Mkdir makes a directory at root/dirname.
func (hfs *HashFS) Mkdir(ctx context.Context, root, dirname string, cmdhash []byte) error {
	hfs.clean.Store(false)
	dirname = filepath.Join(root, dirname)
	dirname = filepath.ToSlash(dirname)
	fi, err := os.Lstat(dirname)
	mtime := time.Now()
	if err == nil && fi.IsDir() {
		err := os.Chtimes(dirname, time.Time{}, mtime)
		if err != nil {
			log.Warnf("failed to set dir mtime %s: %v: %v", dirname, mtime, err)
		}
	} else {
		err := os.MkdirAll(dirname, 0755)
		if err != nil {
			return err
		}
		fi, err := os.Lstat(dirname)
		if err != nil {
			return err
		}
		if mtime.Before(fi.ModTime()) {
			mtime = fi.ModTime()
		}
	}
	lready := make(chan bool, 1)
	lready <- true

	e := &entry{
		lready:      lready,
		mtime:       mtime,
		mode:        0644 | fs.ModeDir,
		cmdhash:     cmdhash,
		directory:   &directory{},
		updatedTime: time.Now(),
		isChanged:   true,
	}
	err = hfs.dirStoreAndNotify(ctx, dirname, e)
	var serr storeRaceError
	if errors.As(err, &serr) {
		curEntry, ok := serr.curEntry.(*entry)
		if ok {
			// Mkdir succeeds if cur entry is the directory and has the same cmdhash, or cur cmdhash exists but trying to add no cmdhash.
			if curEntry != nil && curEntry.getDir() != nil && (bytes.Equal(cmdhash, curEntry.cmdhash) || (len(curEntry.cmdhash) > 0 && len(cmdhash) == 0)) {
				err = nil
			}
		}
	}
	if err != nil {
		return err
	}
	if len(cmdhash) > 0 {
		hfs.journalEntry(ctx, dirname, e)
	}
	return nil
}

// Remove removes a file at root/fname.
func (hfs *HashFS) Remove(ctx context.Context, root, fname string) error {
	hfs.clean.Store(false)
	fname = filepath.Join(root, fname)
	fname = filepath.ToSlash(fname)
	lready := make(chan bool, 1)
	lready <- true
	e := &entry{
		lready: lready,
		err:    fs.ErrNotExist,
	}
	_, err := hfs.directory.store(ctx, fname, e)
	return err
}

// RemoveAll removes all files under root/name.
// Also removes from the disk at the same time.
func (hfs *HashFS) RemoveAll(ctx context.Context, root, name string) error {
	hfs.clean.Store(false)
	name = filepath.Join(root, name)
	name = filepath.ToSlash(name)
	err := os.RemoveAll(name)
	if err == nil {
		err = fs.ErrNotExist
	}
	lready := make(chan bool, 1)
	lready <- true
	e := &entry{
		lready: lready,
		err:    err,
	}
	_, err = hfs.directory.store(ctx, name, e)
	return err
}

// Forget forgets cached entry for inputs under root.
func (hfs *HashFS) Forget(root string, inputs []string) {
	for _, fname := range inputs {
		fullname := filepath.Join(root, fname)
		fullname = filepath.ToSlash(fullname)
		hfs.directory.delete(fullname)
	}
}

// ForgetMissingsInDir forgets cached entry under root/dir if it isn't
// generated files/dirs by any steps and doesn't exist on local disk.
// It is used for a step that removes files under a dir. b/350662100
func (hfs *HashFS) ForgetMissingsInDir(ctx context.Context, root, dir string) {
	inputs := []string{dir}
	var needCheck []string
	for len(inputs) > 0 {
		fname := inputs[0]
		copy(inputs, inputs[1:])
		inputs = inputs[:len(inputs)-1]
		fi, err := hfs.Stat(ctx, root, fname)
		if errors.Is(err, fs.ErrNotExist) {
			// If it doesn't exist in hashfs,
			// no need to check more.
			continue
		}
		if err == nil {
			if fi.IsDir() {
				dents, err := hfs.ReadDir(ctx, root, fname)
				if err != nil {
					log.Warnf("readdir failed for %q: %v", fname, err)
					continue
				}
				for _, dent := range dents {
					inputs = append(inputs, filepath.ToSlash(filepath.Join(fname, dent.Name())))
				}
			}
			if fi.IsChanged() {
				// it is explicitly generated file/dir,
				// no need to check more.
				continue
			}
		}
		needCheck = append(needCheck, fname)
	}
	for _, fname := range needCheck {
		fullname := filepath.Join(root, fname)
		fullname = filepath.ToSlash(fullname)
		_, err := os.Lstat(fullname)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				hfs.directory.delete(fullname)
				continue
			}
			log.Errorf("lstat failed for %q: %v", fullname, err)
		}
	}
}

// Availables returns valid inputs (i.e. exist in hashfs).
func (hfs *HashFS) Availables(ctx context.Context, root string, inputs []string) []string {
	availables := make([]string, 0, len(inputs))
	for _, fname := range inputs {
		_, err := hfs.Stat(ctx, root, fname)
		if errors.Is(err, fs.ErrNotExist) {
			// If it doesn't exist in hashfs,
			// no need to check with os.Lstat.
			continue
		}
		availables = append(availables, fname)
	}
	return availables
}

// Entries gets merkletree entries for inputs at root.
// it won't return entries symlink escaped from root.
func (hfs *HashFS) Entries(ctx context.Context, root string, inputs []string) ([]merkletree.Entry, error) {
	var nwait int
	var wg sync.WaitGroup
	ents := make([]*entry, 0, len(inputs))
	for _, fname := range inputs {
		fname := filepath.Join(root, fname)
		fname = filepath.ToSlash(fname)
		e, _, ok := hfs.directory.lookup(ctx, fname)
		if ok {
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
						e.compute(ctx)
					}()
				}
			}
			continue
		}
		e = newLocalEntry()
		e.init(fname, hfs.executables)
		if errors.Is(e.err, context.Canceled) {
			return nil, e.err
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
			e.compute(ctx)
		}()
	}
	// wait ensures all entries have computed the digests.
	wg.Wait()
	var entries []merkletree.Entry
	for i, fname := range inputs {
		e := ents[i]
		d := e.digest()
		if e.err != nil || (d.IsZero() && e.target == "" && e.directory == nil) {
			// TODO: hard fail instead?
			continue
		}
		data := digest.NewData(e.src, d)
		isExecutable := e.mode&0111 != 0
		target := e.target
		if target != "" {
			var tname string
			name := filepath.Join(root, fname)
			elink := e
			for range maxSymlinks {
				if filepath.IsAbs(elink.target) {
					tname = elink.target
				} else {
					tname = filepath.Join(filepath.Dir(name), elink.target)
				}
				tname = filepath.ToSlash(tname)
				if strings.HasPrefix(tname, root+"/") {
					break
				}
				// symlink to out of exec root (e.g. ../.cipd/pkgs/..)
				name = tname
				tname = ""
				var ok bool
				elink, _, ok = hfs.directory.lookup(ctx, name)
				if !ok {
					elink = newLocalEntry()
					elink.init(name, hfs.executables)
					var err error
					elink, err = hfs.directory.store(ctx, name, elink)
					if err != nil {
						return nil, err
					}
					go elink.compute(ctx)
				}
				if elink.err != nil || elink.target == "" {
					break
				}
			}
			if e != elink {
				target = elink.target
				elink.compute(ctx)
				d := elink.digest()
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

// UpdateEntry is an entry for Update.
type UpdateEntry struct {
	Name string

	// if Entry is nil, use local disk (from RetrieveUpdateEntriesFromLocal), so need to calculate digest from file.
	// If Entry is not nil, use digest in Entry, rather than calculating digest from file.
	Entry *merkletree.Entry

	Mode        fs.FileMode
	ModTime     time.Time
	CmdHash     []byte
	Action      digest.Digest
	UpdatedTime time.Time
	IsChanged   bool

	// IsLocal=true uses Entry, but assumes file exists on local,
	// to avoid unnecessary flush operation.
	IsLocal bool
}

func (e UpdateEntry) String() string {
	var buf strings.Builder
	fmt.Fprintf(&buf, "%q ", e.Name)
	if e.Entry != nil {
		switch {
		case !e.Entry.Data.IsZero():
			fmt.Fprintf(&buf, "file=%s ", e.Entry.Data.Digest())
		case e.Entry.Target != "":
			fmt.Fprintf(&buf, "symlink=%s ", e.Entry.Target)
		default:
			fmt.Fprintf(&buf, "dir ")
		}
	} else {
		fmt.Fprintf(&buf, "local ")
	}
	fmt.Fprintf(&buf, "mode=%s ", e.Mode)
	fmt.Fprintf(&buf, "mtime=%s ", e.ModTime.Format(time.RFC3339Nano))
	fmt.Fprintf(&buf, "cmdhash=%s ", base64.StdEncoding.EncodeToString(e.CmdHash))
	fmt.Fprintf(&buf, "action=%s ", e.Action)
	if !e.ModTime.Equal(e.UpdatedTime) {
		fmt.Fprintf(&buf, "updated_time=%s ", e.UpdatedTime.Format(time.RFC3339Nano))
	}
	fmt.Fprintf(&buf, "changed=%t is_local=%t", e.IsChanged, e.IsLocal)
	return buf.String()
}

// Update updates cache information for entries under execRoot.
func (hfs *HashFS) Update(ctx context.Context, execRoot string, entries []UpdateEntry) error {
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	default:
	}
	hfs.clean.Store(false)

	// sort inputs, so update dir containing files first. b/300385880
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name < entries[j].Name
	})

	for _, ent := range entries {
		fname := filepath.Join(execRoot, ent.Name)
		fname = filepath.ToSlash(fname)
		if ent.Entry == nil {
			// UpdateEntry was captured by RetrieveUpdateEntriesFromLocal
			// so the entry should exists in hfs.directory.
			e, _, ok := hfs.dirLookup(ctx, execRoot, ent.Name)
			if !ok {
				log.Warnf("failed to update: no entry %s", ent.Name)
				continue
			}
			if e.getMtime().Equal(ent.ModTime) || e.getDir() != nil {
				e.mu.Lock()
				e.mtime = ent.ModTime
				e.mode = ent.Mode
				e.cmdhash = ent.CmdHash
				e.action = ent.Action
				e.updatedTime = ent.UpdatedTime
				e.isChanged = ent.IsChanged
				e.mu.Unlock()
			} else {
				e = newLocalEntry()
				e.init(fname, hfs.executables)
				e.mtime = ent.ModTime
				e.cmdhash = ent.CmdHash
				e.action = ent.Action
				e.updatedTime = ent.UpdatedTime
				e.isChanged = ent.IsChanged
			}
			if errors.Is(e.err, context.Canceled) {
				return e.err
			}
			// notify this output to scandeps.
			err := hfs.dirStoreAndNotify(ctx, fname, e)
			if err != nil {
				return err
			}
			hfs.journalEntry(ctx, fname, e)
			if ent.IsLocal && e.isChanged && e.target == "" {
				// Update mtime for the local entry if it has changed.
				// Don't update mtime for symlink,
				// since os.Chtimes updates the mtime of target
				// and it makes the target invalidated
				// in .siso_fs_state since mtime doesn't match.
				err := os.Chtimes(fname, time.Time{}, e.getMtime())
				if errors.Is(err, fs.ErrNotExist) {
					log.Warnf("failed to update mtime of %s: %v", fname, err)
					continue
				}
				if err != nil {
					return fmt.Errorf("failed to update mtime of %s: %w", fname, err)
				}
			}
			continue
		}
		switch {
		case !ent.Entry.Data.IsZero():
			lready := make(chan bool, 1)
			if ent.IsLocal {
				close(lready)
			} else {
				lready <- true
			}
			mode := ent.Mode
			if ent.Entry.IsExecutable {
				mode |= 0111
			}
			e := &entry{
				lready:      lready,
				size:        ent.Entry.Data.Digest().SizeBytes,
				mtime:       ent.ModTime,
				mode:        mode,
				cmdhash:     ent.CmdHash,
				action:      ent.Action,
				src:         ent.Entry.Data,
				d:           ent.Entry.Data.Digest(),
				updatedTime: ent.UpdatedTime,
				isChanged:   ent.IsChanged,
			}
			err := hfs.dirStoreAndNotify(ctx, fname, e)
			if err != nil {
				return err
			}
			hfs.journalEntry(ctx, fname, e)
			if ent.IsLocal && e.isChanged {
				err = os.Chtimes(fname, time.Time{}, e.getMtime())
				if errors.Is(err, fs.ErrNotExist) {
					log.Warnf("failed to update mtime of %s: %v", fname, err)
					continue
				}
				if err != nil {
					return fmt.Errorf("failed to update mtime of %s: %w", fname, err)
				}
			}
		case ent.Entry.Target != "":
			lready := make(chan bool, 1)
			if ent.IsLocal {
				close(lready)
			} else {
				lready <- true
			}
			mode := ent.Mode
			mode |= fs.ModeSymlink
			e := &entry{
				lready:  lready,
				mtime:   ent.ModTime,
				mode:    mode,
				cmdhash: ent.CmdHash,
				action:  ent.Action,
				target:  ent.Entry.Target,

				updatedTime: ent.UpdatedTime,
				isChanged:   ent.IsChanged,
			}
			err := hfs.dirStoreAndNotify(ctx, fname, e)
			if err != nil {
				return err
			}
			hfs.journalEntry(ctx, fname, e)
		default: // directory
			lready := make(chan bool, 1)
			if ent.IsLocal {
				close(lready)
			} else {
				lready <- true
			}
			mode := ent.Mode
			mode |= fs.ModeDir
			e := &entry{
				lready:    lready,
				mtime:     ent.ModTime,
				mode:      mode,
				cmdhash:   ent.CmdHash,
				action:    ent.Action,
				directory: &directory{},

				updatedTime: ent.UpdatedTime,
				isChanged:   ent.IsChanged,
			}
			err := hfs.dirStoreAndNotify(ctx, fname, e)
			if err != nil {
				return err
			}
			hfs.journalEntry(ctx, fname, e)
			err = os.Chtimes(fname, time.Time{}, ent.ModTime)
			if err != nil {
				log.Warnf("failed to update dir mtime %s: %v", fname, err)
			}
		}
	}
	return nil
}

// RetrieveUpdateEntries gets UpdateEntry for fnames at root.
func (hfs *HashFS) RetrieveUpdateEntries(ctx context.Context, root string, fnames []string) []UpdateEntry {
	ents, err := hfs.Entries(ctx, root, fnames)
	if err != nil {
		log.Warnf("failed to get entries: %v", err)
	}
	entries := make([]UpdateEntry, 0, len(ents))
	for _, ent := range ents {
		fi, err := hfs.Stat(ctx, root, ent.Name)
		if err != nil {
			log.Warnf("failed to stat %s: %v", ent.Name, err)
			continue
		}
		entries = append(entries, UpdateEntry{
			Name:        ent.Name,
			Entry:       &ent,
			Mode:        fi.Mode(),
			ModTime:     fi.ModTime(),
			CmdHash:     fi.CmdHash(),
			Action:      fi.Action(),
			UpdatedTime: fi.UpdatedTime(),
			IsChanged:   fi.IsChanged(),
		})
	}
	return entries
}

// RetrieveUpdateEntriesFromLocal gets UpdateEntry for fnames at root from local disk.
// It is intended to be used for local execution outputs in cmd.RecordOutputsFromLocal.
// It won't wait for digest calculation for entries, so UpdateEntry's Entry
// will be nil.
// It will forget recorded enties when err (doesn't exist or so).
func (hfs *HashFS) RetrieveUpdateEntriesFromLocal(ctx context.Context, root string, fnames []string) []UpdateEntry {
	ents := make([]UpdateEntry, 0, len(fnames))
	for _, fname := range fnames {
		fullname := filepath.Join(root, fname)
		fullname = filepath.ToSlash(fullname)
		lfi, err := os.Lstat(fullname)
		if errors.Is(err, fs.ErrNotExist) {
			log.Warnf("missing local %s: %v", fname, err)
			hfs.directory.delete(fullname)
			continue
		} else if err != nil {
			log.Warnf("failed to access local %s: %v", fname, err)
			hfs.directory.delete(fullname)
			continue
		}
		if !lfi.IsDir() {
			// forget old entries unless dir.
			// need to keep dir to keep other files in the dir.
			hfs.directory.delete(fullname)
		}
		ent := UpdateEntry{
			Name:    fname,
			Mode:    lfi.Mode(),
			ModTime: lfi.ModTime(),
			IsLocal: true,
		}
		fi, err := hfs.Stat(ctx, root, fname)
		if err != nil {
			log.Warnf("failed to stat after invalidate %s: %v", fname, err)
		} else {
			ent.CmdHash = fi.CmdHash()
			ent.Action = fi.Action()
			ent.UpdatedTime = fi.UpdatedTime()
			ent.IsChanged = fi.IsChanged()
		}
		ents = append(ents, ent)
	}
	return ents
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

func (noDataSource) Source(_ context.Context, d digest.Digest, fname string) digest.Source {
	return noSource{fname}
}

// NeedFlush returns whether the fname need to be flushed based on OutputLocal option.
func (hfs *HashFS) NeedFlush(ctx context.Context, execRoot, fname string) bool {
	return hfs.opt.OutputLocal(ctx, filepath.ToSlash(filepath.Join(execRoot, fname)))
}

// Flush flushes cached information for files under execRoot to local disk.
func (hfs *HashFS) Flush(ctx context.Context, execRoot string, files []string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	eg, ctx := errgroup.WithContext(ctx)
	for _, file := range files {
		fname := filepath.Join(execRoot, file)
		fname = filepath.ToSlash(fname)
		e, _, ok := hfs.directory.lookup(ctx, fname)
		if !ok {
			// If it doesn't exist in memory, just use local disk as is.
			continue
		}
		select {
		case need := <-e.lready:
			if !need {
				// need=false means file is already downloaded,
				// or entry was constructed from local disk.
				e.mu.Lock()
				if e.mtimeUpdated && e.target == "" {
					// mtime was updated after entry sets mtime from the local disk.
					// Don't update mtime for symlink,
					// since os.Chtimes updates the mtime of target
					// and it makes the target invalidated
					// in .siso_fs_state since mtime doesn't match.
					err := os.Chtimes(fname, time.Time{}, e.mtime)
					if err == nil {
						e.mtimeUpdated = false
					}
				}
				e.mu.Unlock()
				err := e.err
				if errors.Is(err, fs.ErrNotExist) {
					// TODO: hard fail?
					continue
				}
				if err != nil {
					return fmt.Errorf("flush %s local-ready: %w", fname, err)
				}
				continue
			}
		case <-ctx.Done():
			return fmt.Errorf("flush %s: %w", fname, context.Cause(ctx))
		}
		e.compute(ctx)
		eg.Go(func() (err error) {
			err = e.flush(ctx, fname)
			// flush should not fail with cas not found error.
			// but if it failed, current recorded digest should
			// be wrong, so should delete from the hashfs.
			if code := status.Code(err); code == codes.NotFound {
				log.Warnf("flush failed. delete %s from hashfs: %v", fname, err)
				hfs.directory.delete(fname)
			}
			return err
		})
	}
	return eg.Wait()
}

// Refresh refreshes cached file entries under execRoot.
func (hfs *HashFS) Refresh(ctx context.Context, execRoot string) error {
	// TODO: optimize?
	state := hfs.State(ctx)
	// reset loaded as it reset entry data.
	hfs.loaded.Store(false)
	hfs.directory = &directory{isRoot: true}
	err := hfs.SetState(ctx, state)
	werr := hfs.WaitReady(ctx)
	if err != nil {
		return err
	}
	return werr
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

	// isChanged indicates the file is changed in the session.
	isChanged bool

	target string // symlink.

	src digest.Source
	buf []byte // from WriteFile.

	mu sync.Mutex
	// mtime of entry in hashfs.
	mtime        time.Time
	mtimeUpdated bool
	// updatedTime is timestamp when the file has been updated
	// by Update or UpdateFromLocal.
	// need to distinguish from mtime for restat=1.
	// updatedTime should be equal or newer than mtime.
	updatedTime time.Time

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

func (e *entry) init(fname string, executables map[string]bool) {
	fi, err := os.Lstat(fname)
	if errors.Is(err, fs.ErrNotExist) {
		e.err = err
		return
	}
	if err != nil {
		log.Warnf("failed to lstat %s: %v", fname, err)
		e.err = err
		return
	}
	if now := time.Now(); fi.ModTime().After(now) {
		log.Warnf("future timestamp on %s: mtime=%s now=%s", fname, fi.ModTime(), now)
		e.err = fmt.Errorf("future timestamp on %s: mtime=%s now=%s", fname, fi.ModTime(), now)
		return
	}
	switch {
	case fi.IsDir():
		e.directory = &directory{}
		e.mode = 0644 | fs.ModeDir
	case fi.Mode().Type() == fs.ModeSymlink:
		e.mode = 0644 | fs.ModeSymlink
		e.target, err = os.Readlink(fname)
		if err != nil {
			e.err = err
		}
	case fi.Mode().IsRegular():
		e.mode = 0644
		if isExecutable(fi, fname, executables) {
			e.mode |= 0111
		}
		e.size = fi.Size()
		e.src = osfs.NewFileSource(fname)
	default:
		e.err = fmt.Errorf("unexpected filetype not regular %s: %s", fi.Mode(), fname)
		log.Errorf("tree entry %s: unknown filetype %s", fname, fi.Mode())
		return
	}
	if e.mtime.Before(fi.ModTime()) {
		e.mtime = fi.ModTime()
	}
	if e.updatedTime.Before(e.mtime) {
		e.updatedTime = e.mtime
	}
}

func (e *entry) compute(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.err != nil {
		return e.err
	}
	if !e.d.IsZero() {
		return nil
	}
	if e.src == nil {
		return nil
	}
	data, err := digest.FromSource(ctx, e.src)
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

func (e *entry) getUpdatedTime() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.updatedTime
}

func (e *entry) updateDir(ctx context.Context, hfs *HashFS, dname string) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	d, err := os.Open(dname)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Warnf("updateDir %s: open %v", dname, err)
		}
		return nil
	}
	defer d.Close()
	fi, err := d.Stat()
	if err != nil {
		log.Warnf("updateDir %s: stat %v", dname, err)
		return nil
	}
	if !fi.IsDir() {
		log.Warnf("updateDir %s: is not dir?", dname)
		return nil
	}
	if fi.ModTime().Equal(e.directory.mtime) {
		return nil
	}
	names, err := d.Readdirnames(-1)
	if err != nil {
		log.Warnf("updateDir %s: readdirnames %v", dname, err)
		return nil
	}
	for _, name := range names {
		if hfs.opt.Ignore(ctx, filepath.Join(dname, name)) {
			continue
		}
		// update entry in e.directory.
		_, err := hfs.Stat(ctx, dname, name)
		if err != nil {
			log.Warnf("updateDir stat %s: %v", name, err)
		}
	}
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

func (e *entry) flush(ctx context.Context, fname string) error {
	defer close(e.lready)

	if errors.Is(e.err, fs.ErrNotExist) {
		return os.Remove(fname)
	}
	d := e.digest()
	mtime := e.getMtime()
	switch {
	case e.directory != nil:
		// directory
		fi, err := os.Lstat(fname)
		if err == nil && fi.IsDir() && fi.ModTime().Equal(mtime) {
			return nil
		}
		err = os.MkdirAll(fname, 0755)
		if err != nil {
		} else {
			err = os.Chtimes(fname, time.Time{}, mtime)
		}
		return err
	case d.IsZero() && e.target != "":
		target, err := os.Readlink(fname)
		if err == nil && e.target == target {
			return nil
		}
		e.mu.Lock()
		err = os.Symlink(e.target, fname)
		if errors.Is(err, fs.ErrExist) {
			err = os.Remove(fname)
			if err == nil {
				err = os.Symlink(e.target, fname)
			}
		}
		e.mu.Unlock()
		// don't change mtimes. it fails if target doesn't exist.
		return err
	default:
	}
	fi, err := os.Lstat(fname)
	// need to remove the file after it reads from data source,
	// since data source will read from the local disk.
	var removeReason string
	if err == nil {
		if fi.IsDir() {
			err := &fs.PathError{
				Op:   "flush",
				Path: fname,
				Err:  syscall.EISDIR,
			}
			log.Warnf("flush %s: %v", fname, err)
			return err
		}
		if fi.Size() == d.SizeBytes && fi.ModTime().Equal(mtime) {
			// TODO: check hash, mode?
			return nil
		}
		if isHardlink(fi) {
			removeReason = "hardlink"
		} else if !fi.Mode().IsRegular() {
			removeReason = fmt.Sprintf("non-regular file %s", fi.Mode())
		} else if fi.Size() == d.SizeBytes {
			// check existing file and hashfs entry are identical
			// or not by checking digest.
			// if size differs, no need to check and force
			// to write hashfs entry to the disk.
			var fileDigest digest.Digest
			src := osfs.NewFileSource(fname)
			ld, err := digest.FromSource(ctx, src)
			if err == nil {
				fileDigest = ld.Digest()
				if fileDigest == d {
					if !fi.ModTime().Equal(mtime) {
						err = os.Chtimes(fname, time.Time{}, mtime)
					}
					return err
				}
			}
			log.Warnf("flush %s: exists but mismatch size:%d!=%d mtime:%s!=%s d:%v!=%v", fname, fi.Size(), d.SizeBytes, fi.ModTime(), mtime, fileDigest, d)
		}
		if removeReason == "" && fi.Mode()&0200 == 0 {
			// need to be writable. otherwise os.WriteFile fails with permission denied.
			err = os.Chmod(fname, fi.Mode()|0200)
			log.Warnf("flush %s: not writable? %s: %v", fname, fi.Mode(), err)
		}
	}
	err = os.MkdirAll(filepath.Dir(fname), 0755)
	if err != nil {
		log.Warnf("flush %s: mkdir: %v", fname, err)
		return fmt.Errorf("failed to create directory for %s: %w", fname, err)
	}
	if d.SizeBytes == 0 {
		if removeReason != "" {
			err = os.Remove(fname)
		}
		err := os.WriteFile(fname, nil, 0644)
		if err != nil {
			return err
		}
		err = os.Chtimes(fname, time.Time{}, mtime)
		if err != nil {
			return err
		}
		return nil
	}
	buf := e.buf
	removeBeforeWrite := func() {
		if removeReason != "" {
			err = os.Remove(fname)
		}
	}
	if len(buf) == 0 {
		if e.d.IsZero() {
			return fmt.Errorf("no data: retrieve %s: ", fname)
		}
		err = func() error {
			// check if hashfs entry is copy of local file,
			// i.e. created by hashfs Copy method.
			// if hashfs entry is set by remote action,
			// it would not be NewFileSource
			lsrc, ok := e.src.(osfs.FileSource)
			if ok && osfs.HasClonefile {
				if lsrc.Fname == fname {
					return os.Chmod(fname, e.mode)
				}
				removeBeforeWrite()
				err := osfs.Clonefile(lsrc.Fname, fname)
				if err == nil {
					return os.Chmod(fname, e.mode)
				}
				// clonefile err, fallback to normal copy
				log.Warnf("clonefile failed: %v", err)
			}
			// write into tmp and rename after remove.
			// e.src may be the same as fname, but
			// we may need to remove fname for some reason
			// (hardlink etc).
			tmpname := filepath.Join(filepath.Dir(fname), "."+filepath.Base(fname)+".tmp")

			r, err := e.src.Open(ctx)
			if err != nil {
				return err
			}
			defer r.Close()
			w, err := os.OpenFile(tmpname, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, e.mode)
			if err != nil {
				return err
			}
			_, err = io.Copy(w, r)
			cerr := w.Close()
			if err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}

			removeBeforeWrite()
			return os.Rename(tmpname, fname)
		}()
	} else {
		removeBeforeWrite()
		err = os.WriteFile(fname, buf, e.mode)
	}
	if err != nil {
		return err
	}
	err = os.Chtimes(fname, time.Time{}, mtime)
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

	// isRoot is true if its' the root directory of the hashfs.
	isRoot bool
}

func (d *directory) String() string {
	if d == nil {
		return "<nil>"
	}
	// better to dump all entries?
	return fmt.Sprintf("&directory{m:%p}", &d.m)
}

// path elements of filepath.
// defer allocation for lookup, but pass elems for store.
type pathElements struct {
	origFname string

	// number of elements processed.
	n int

	// elements processed. maybe empty for lookup
	elems []string
}

func (d *directory) lookup(ctx context.Context, fname string) (*entry, *directory, bool) {
	// expect d.isRoot == true
	for range maxSymlinks {
		e, dir, resolved, ok := d.lookupEntry(ctx, fname)
		if e != nil || dir != nil {
			return e, dir, ok
		}
		if resolved != "" {
			if !d.isRoot {
				log.Warnf("hashfs directory lookup must be called from root directory")
			}
			fname = resolved
			continue
		}
		return nil, nil, false
	}
	return nil, nil, false
}

func (d *directory) lookupEntry(ctx context.Context, fname string) (*entry, *directory, string, bool) {
	pe := pathElements{
		origFname: fname,
	}
	for fname != "" {
		fname = strings.TrimPrefix(fname, "/")
		elem, rest, ok := strings.Cut(fname, "/")
		if !ok {
			e, ok := d.m.Load(fname)
			if !ok {
				return nil, d, "", false
			}
			return e.(*entry), d, "", true
		}
		fname = rest
		pe.n++
		subdir, target, ok := resolveNextDir(ctx, d, lookupNextDir, pe, elem, fname)
		if subdir == nil {
			if target != "" {
				return nil, nil, target, false
			}
			if !ok {
				return nil, nil, "", false
			}
		}
		d = subdir
	}
	return nil, nil, "", false
}

var errRootSymlink = errors.New("symlink resolved from root")

func (d *directory) store(ctx context.Context, fname string, e *entry) (*entry, error) {
	for range maxSymlinks {
		ent, resolved, err := d.storeEntry(ctx, fname, e)
		if resolved != "" {
			if !d.isRoot {
				if filepath.IsAbs(resolved) {
					return nil, fmt.Errorf("root symlink %s: %w", resolved, errRootSymlink)
				}
				if !filepath.IsLocal(resolved) {
					return nil, fmt.Errorf("non local symlink %s: %w", resolved, errRootSymlink)
				}
			}
			fname = resolved
			continue
		}
		return ent, err
	}
	return nil, fmt.Errorf("store %s: %w", fname, syscall.ELOOP)
}

type storeRaceError struct {
	fname     string
	prevEntry *entry
	entry     *entry
	curEntry  any // *entry
	exists    bool
}

func (e storeRaceError) Error() string {
	return fmt.Sprintf("store race %s: %p -> %p -> %p %t", e.fname, e.prevEntry, e.entry, e.curEntry, e.exists)
}

func (d *directory) storeEntry(ctx context.Context, fname string, e *entry) (*entry, string, error) {
	pe := pathElements{
		origFname: fname,
		elems:     make([]string, 0, strings.Count(fname, "/")+1),
	}
	if strings.HasPrefix(fname, "/") {
		pe.elems = append(pe.elems, "/")
	}
	for fname != "" {
		fname = strings.TrimPrefix(fname, "/")
		elem, rest, ok := strings.Cut(fname, "/")
		if !ok {
			v, loaded := d.m.LoadOrStore(fname, e)
			if !loaded {
				return e, "", nil
			}
			// check whether there is an update from previous entry.
			ee := v.(*entry)
			if e == ee {
				// if storing entry `e` is the same as stored entry `ee`, no need to update.
				return e, "", nil
			}
			eed := ee.digest()
			// old entry has cmdhash, but new entry has no cmdhash&action (not by Update*).
			if len(ee.cmdhash) > 0 && len(e.cmdhash) == 0 && e.action.IsZero() {
				// keep cmdhash and action
				e.cmdhash = ee.cmdhash
				e.action = ee.action
			}
			cmdchanged := !bytes.Equal(ee.cmdhash, e.cmdhash)
			actionchanged := ee.action != e.action
			if e.target != "" && ee.target != e.target {
				// ???
			} else if !e.d.IsZero() && eed != e.d && eed.SizeBytes != 0 && e.d.SizeBytes != 0 {
				// don't log nil to digest of empty file (size=0)
			} else if cmdchanged || actionchanged {
				// ???
			} else if ee.target == e.target && ee.size == e.size && ee.mode == e.mode && (e.d.IsZero() || eed == e.d) {
				// no change?

				// if e.d is zero, it may be new local entry
				// and ee.d has been calculated

				// update mtime and updatedTime.
				ee.mu.Lock()
				ee.mtimeUpdated = !ee.mtime.Equal(e.mtime)
				ee.mtime = e.mtime
				if ee.updatedTime.Before(e.updatedTime) {
					ee.updatedTime = e.updatedTime
				}
				ee.isChanged = e.isChanged
				ee.mu.Unlock()
				return ee, "", nil
			} else if ee.getDir() != nil && e.getDir() != nil {
				// ok if mkdir with the no cmdhash or same cmdhash.
				if (len(ee.cmdhash) > 0 && len(e.cmdhash) == 0) || bytes.Equal(ee.cmdhash, e.cmdhash) {
					return ee, "", nil
				}
			}

			// e should be new value for fname.
			swapped := d.m.CompareAndSwap(fname, ee, e)
			if !swapped {
				// store race?
				v, ok := d.m.Load(fname)
				return nil, "", storeRaceError{
					fname:     fname,
					prevEntry: ee,
					entry:     e,
					curEntry:  v,
					exists:    ok,
				}
			}
			// e is stored for fname
			return e, "", nil

		}
		pe.n++
		pe.elems = append(pe.elems, elem)
		fname = rest
		subdir, resolved, ok := resolveNextDir(ctx, d, nextDir, pe, elem, fname)
		if resolved != "" {
			return nil, resolved, nil
		}
		if !ok {
			return nil, "", fmt.Errorf("store resolve next dir %s failed: %s", elem, pe.origFname)
		}
		d = subdir
	}
	errOrigFname := pe.origFname
	return nil, "", fmt.Errorf("bad fname? %q", errOrigFname)
}

// resolveNextDir resolves a dir named `elem` by calling `next`.
// `next` will return *directory if `elem` entry is directory.
// `next` will return string if `elem` entry is symlink.
// resolveNextDir returns directory if resolved `elem` is directory.
// resolveNextDir returns resolved path name as string if resolved `elem` is symlink.
func resolveNextDir(ctx context.Context, d *directory, next func(context.Context, *directory, pathElements, string) (*directory, string, bool), pe pathElements, elem, rest string) (*directory, string, bool) {
	for range maxSymlinks {
		nextDir, target, ok := next(ctx, d, pe, elem)
		if target != "" {
			if len(pe.elems) != pe.n {
				// reconstruct elems for lookup
				pe.elems = make([]string, 0, pe.n+1)
				if strings.HasPrefix(pe.origFname, "/") {
					pe.elems = append(pe.elems, "/")
				}
				s := pe.origFname
				for range pe.n - 1 {
					s = strings.TrimPrefix(s, "/")
					elem, rest, _ := strings.Cut(s, "/")
					pe.elems = append(pe.elems, elem)
					s = rest
				}
				pe.elems = append(pe.elems, elem)
			}
			if filepath.IsAbs(target) {
				resolved := filepath.ToSlash(filepath.Join(target, rest))
				return nil, resolved, false
			}
			pe.elems[len(pe.elems)-1] = target
			pe.elems = append(pe.elems, rest)
			resolved := filepath.ToSlash(filepath.Join(pe.elems...))
			return nil, resolved, false
		}

		if !ok {
			return nil, "", false
		}
		if nextDir != nil {
			return nextDir, "", true
		}
	}
	return nil, "", false
}

// next for lookup case.
func lookupNextDir(ctx context.Context, d *directory, pe pathElements, elem string) (*directory, string, bool) {
	v, ok := d.m.Load(elem)
	if !ok {
		return nil, "", false
	}
	dent := v.(*entry)
	if dent != nil {
		if dent.err != nil {
			return nil, "", false
		}
		target := dent.target
		subdir := dent.getDir()
		if subdir == nil && target == "" {
			return nil, "", false
		}
		return subdir, target, true
	}
	return nil, "", false
}

// next for store case.
// nextDir will create next dir entry if needed.
func nextDir(ctx context.Context, d *directory, pe pathElements, elem string) (*directory, string, bool) {
	v, ok := d.m.Load(elem)
	if ok {
		dent := v.(*entry)
		if dent != nil && dent.err == nil {
			target := dent.target
			subdir := dent.getDir()
			if subdir == nil && target == "" {
				return nil, "", false
			}
			return subdir, target, true
		}
		_ = d.m.CompareAndDelete(elem, dent)
	}
	// create intermediate dir of elem.
	mtime := time.Now()
	fullname := filepath.Join(pe.elems...)
	dfi, err := os.Lstat(fullname)
	if err == nil {
		mtime = dfi.ModTime()
		switch {
		case dfi.IsDir():
		case dfi.Mode().Type() == fs.ModeSymlink:
			target, err := os.Readlink(fullname)
			if err != nil {
				log.Warnf("readlink %s: %v", fullname, err)
				return nil, "", false
			}
			lready := make(chan bool, 1)
			lready <- true
			newDent := &entry{
				lready: lready,
				mode:   0644 | fs.ModeSymlink,
				mtime:  mtime,
				target: target,
			}
			dent := newDent
			v, ok := d.m.LoadOrStore(elem, dent)
			if ok {
				dent = v.(*entry)
			}
			if dent.mode != newDent.mode || dent.target != newDent.target {
				log.Warnf("store %s symlink dir: race? store %s %s / loaded %s %s", pe.origFname, newDent.mode, newDent.target, dent.mode, dent.target)
			}
			return nil, target, true
		default:
			log.Warnf("unexpected mode %s: %s", fullname, dfi.Mode().Type())
			return nil, "", false
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
	d = subdir
	if d == nil && target == "" {
		log.Warnf("store %s no dir, no symlink", pe.origFname)
		return nil, "", false
	}
	return d, target, true
}

func (d *directory) delete(fname string) {
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

// UpdatedTime is a update time of the file.
// Usually it is the same with ModTime, but may differ for restat=1.
func (fi FileInfo) UpdatedTime() time.Time {
	return fi.e.getUpdatedTime()
}

// IsChanged returns true if file has been changed in the session.
func (fi FileInfo) IsChanged() bool {
	fi.e.mu.Lock()
	defer fi.e.mu.Unlock()
	return fi.e.isChanged
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
