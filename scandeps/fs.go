// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package scandeps

import (
	"context"
	"fmt"
	"hash/maphash"
	"path/filepath"
	"strings"
	"sync"

	log "github.com/golang/glog"

	"infra/build/siso/hashfs"
	"infra/build/siso/o11y/clog"
)

// filesystem is mirror of hashfs to optimize for scandeps access pattern.
// it is shared for all scandeps processes.
// without this, hashfs would have lots of negative caches for non-existing
// header files for every include directory.
type filesystem struct {
	hashfs *hashfs.HashFS

	// shard by basename to reduce lock contention
	dirs  sync.Map // basename -> dir -> []dirents
	files sync.Map // basename -> files -> *scanresult

	dircache sync.Map // dir -> base -> bool

	// shard by maphash to reduce lock contention
	symtab  [256]sync.Map  // for incname, macros
	pathtab [4096]sync.Map // for pathname
	seed    maphash.Seed
}

type dircache struct {
	ready chan struct{}
	m     sync.Map
	err   error
}

// update updates filesystem modification by fi.
func (fsys *filesystem) update(ctx context.Context, fi *hashfs.FileInfo) {
	if log.V(1) {
		clog.Infof(ctx, "update %s dir:%t", fi.Path(), fi.IsDir())
	}
	var dname string
	var base string
	if !fi.IsDir() {
		fname := filepath.ToSlash(fi.Path())
		fsys.forgetFile(fname)
		base = filepath.Base(fname)
		dname = filepath.ToSlash(filepath.Dir(fname))
	} else {
		dname = filepath.ToSlash(fi.Path())
	}
	// fix dircache
	for dname := dname; !strings.HasSuffix(dname, "/"); {
		v, ok := fsys.dircache.Load(dname)
		if !ok {
			base = filepath.Base(dname)
			dname = filepath.ToSlash(filepath.Dir(dname))
			continue
		}
		dc := v.(*dircache)
		select {
		case <-dc.ready:
		default:
			clog.Infof(ctx, "update race ReadDir&update %s", fi.Path())
			fsys.dircache.Delete(dname)
			base = filepath.Base(dname)
			dname = filepath.ToSlash(filepath.Dir(dname))
			continue
		}
		if dc.err != nil {
			// negative cache?
			clog.Infof(ctx, "update clear negative cache %s %v", fi.Path(), dc.err)
			fsys.dircache.Delete(dname)
			base = filepath.Base(dname)
			dname = filepath.ToSlash(filepath.Dir(dname))
			continue
		}
		if base != "" {
			dc.m.LoadOrStore(base, true)
		}
		base = filepath.Base(dname)
		dname = filepath.ToSlash(filepath.Dir(dname))
	}
	for !strings.HasSuffix(dname, "/") {
		if fsys.markDirExists(dname) {
			return
		}
		dname = filepath.ToSlash(filepath.Dir(dname))
	}
}

func (fsys *filesystem) forgetFile(fname string) {
	base := filepath.Base(fname)
	v, ok := fsys.files.Load(base)
	if ok {
		m := v.(*sync.Map)
		m.Delete(fname)
	}
}

func (fsys *filesystem) markDirExists(dname string) bool {
	v, _ := fsys.dirs.LoadOrStore(filepath.Base(dname), new(sync.Map))
	m := v.(*sync.Map)
	v, ok := m.Load(dname)
	if !ok {
		m.Store(dname, true)
		return false
	}
	exist := v.(bool)
	if exist {
		return true
	}
	m.Store(dname, true)
	return false
}

func (fsys *filesystem) ReadDir(ctx context.Context, execRoot, dname string) (*sync.Map, error) {
	fullpath := filepath.ToSlash(filepath.Join(execRoot, dname))
	dv, loaded := fsys.dircache.LoadOrStore(fullpath, &dircache{
		ready: make(chan struct{}),
	})
	dc := dv.(*dircache)
	if !loaded {
		go func() {
			dents, err := fsys.hashfs.ReadDir(ctx, execRoot, dname)
			dc.err = err
			for _, de := range dents {
				dc.m.Store(fsys.pathIntern(de.Name()), true)
			}
			close(dc.ready)
		}()
	}
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("readdirnames[wait]: %w", context.Cause(ctx))
	case <-dc.ready:
	}
	if dc.err != nil {
		return nil, dc.err
	}
	return &dc.m, nil
}

func (fsys *filesystem) intern(v string) string {
	v = strings.Clone(v)
	i := int(maphash.String(fsys.seed, v) % uint64(len(fsys.symtab)))
	vv, _ := fsys.symtab[i].LoadOrStore(v, v)
	return vv.(string)
}

func (fsys *filesystem) pathIntern(v string) string {
	i := int(maphash.String(fsys.seed, v) % uint64(len(fsys.pathtab)))
	vv, _ := fsys.pathtab[i].LoadOrStore(v, v)
	return vv.(string)
}

func (fsys *filesystem) getDir(execRoot, dname string) (exist, ok bool) {
	base := filepath.Base(dname)
	v, _ := fsys.dirs.LoadOrStore(base, new(sync.Map))
	m := v.(*sync.Map)
	v, ok = m.Load(filepath.ToSlash(filepath.Join(execRoot, dname)))
	if !ok {
		return false, false
	}
	exist = v.(bool)
	return exist, true
}

func (fsys *filesystem) setDir(execRoot, dname string, exist bool) {
	v, _ := fsys.dirs.LoadOrStore(filepath.Base(dname), new(sync.Map))
	m := v.(*sync.Map)
	m.Store(filepath.ToSlash(filepath.Join(execRoot, dname)), exist)
}

func (fsys *filesystem) getFile(execRoot, fname string) (*scanResult, bool) {
	v, ok := fsys.files.Load(filepath.Base(fname))
	if !ok {
		return nil, false
	}
	m := v.(*sync.Map)
	v, ok = m.Load(filepath.ToSlash(filepath.Join(execRoot, fname)))
	if !ok {
		return nil, false
	}
	sr := v.(*scanResult)
	return sr, true
}

func (fsys *filesystem) setFile(execRoot, fname string, sr *scanResult) {
	v, _ := fsys.files.LoadOrStore(filepath.Base(fname), new(sync.Map))
	m := v.(*sync.Map)
	m.Store(filepath.ToSlash(filepath.Join(execRoot, fname)), sr)
}
