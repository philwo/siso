// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	log "github.com/golang/glog"

	"infra/build/siso/o11y/clog"
	"infra/build/siso/reapi/digest"
	"infra/build/siso/reapi/merkletree"
)

func treeInputs(ctx context.Context, fn func(context.Context, string) (merkletree.TreeEntry, error), sysroots, dirs []string) []merkletree.TreeEntry {
	var treeEntries []merkletree.TreeEntry
	for _, dir := range sysroots {
		ti, err := fn(ctx, dir)
		if err != nil {
			clog.Warningf(ctx, "treeinput[sysroot] %s: %v", dir, err)
			continue
		}
		treeEntries = append(treeEntries, ti)
	}
	for _, dir := range dirs {
		ti, err := fn(ctx, dir)
		if err != nil {
			if log.V(1) {
				clog.Infof(ctx, "treeinput[dir] %s: %v", dir, err)
			}
			continue
		}
		treeEntries = append(treeEntries, ti)
	}
	return treeEntries
}

func (b *Builder) resolveSymlinkForInputDeps(ctx context.Context, dir, labelSuffix string, inputDeps map[string][]string) (string, []string, error) {
	// Linux imposes a limit of at most 40 symlinks in any one path lookup.
	// see: https://lwn.net/Articles/650786/
	const maxSymlinks = 40
	for i := 0; i < maxSymlinks; i++ {
		files, ok := inputDeps[dir+labelSuffix]
		if ok {
			return dir, files, nil
		}
		fi, err := b.hashFS.Stat(ctx, b.path.ExecRoot, dir)
		if err != nil {
			return "", nil, fmt.Errorf("not in input_deps, and stat err %s: %w", dir, err)
		}
		if target := fi.Target(); target != "" {
			if filepath.IsAbs(target) {
				return "", nil, fmt.Errorf("not in input_deps, and abs symlink %s -> %s", dir, target)
			}
			dir = path.Join(path.Dir(dir), target)
			continue
		}
		return "", nil, fmt.Errorf("not in input_deps %s", dir)
	}
	return "", nil, fmt.Errorf("not in input_deps %s: %w", dir, syscall.ELOOP)
}

func (b *Builder) treeInput(ctx context.Context, dir, labelSuffix string, expandFn func(context.Context, []string) []string) (merkletree.TreeEntry, error) {
	if b.reapiclient == nil {
		return merkletree.TreeEntry{}, errors.New("reapi is not configured")
	}
	m := b.graph.InputDeps(ctx)
	dir, files, err := b.resolveSymlinkForInputDeps(ctx, dir, labelSuffix, m)
	if err != nil {
		return merkletree.TreeEntry{}, err
	}
	st := &subtree{}
	v, _ := b.trees.LoadOrStore(dir, st)
	st = v.(*subtree)
	err = st.init(ctx, b, dir, files, expandFn)
	if err != nil {
		return merkletree.TreeEntry{}, err
	}
	return merkletree.TreeEntry{
		Name:   dir,
		Digest: st.d,
	}, nil
}

type subtree struct {
	once sync.Once
	d    digest.Digest

	mu  sync.Mutex
	err error
}

func (st *subtree) init(ctx context.Context, b *Builder, dir string, files []string, expandFn func(context.Context, []string) []string) error {
	st.once.Do(func() {
		files = b.expandInputs(ctx, files)
		if expandFn != nil {
			files = expandFn(ctx, files)
		}
		var inputs []string
		for _, f := range files {
			if !strings.HasPrefix(f, dir+"/") {
				continue
			}
			inputs = append(inputs, strings.TrimPrefix(f, dir+"/"))
		}
		sort.Strings(inputs)
		ents, err := b.hashFS.Entries(ctx, filepath.Join(b.path.ExecRoot, dir), inputs)
		if err != nil {
			clog.Warningf(ctx, "failed to get subtree entries %s: %v", dir, err)
			st.err = err
			return
		}
		// keep digest in tree in st.ds
		ds := digest.NewStore()
		mt := merkletree.New(ds)
		for _, ent := range ents {
			err := mt.Set(ent)
			if err != nil {
				clog.Warningf(ctx, "failed to set %v: %v", ent, err)
				st.err = err
				return
			}
		}
		st.d, err = mt.Build(ctx)
		if err != nil {
			clog.Warningf(ctx, "failed to build subtree %s: %v", dir, err)
			st.err = err
			return
		}
		// now subtree's digest is ready to use, but
		// file's digests in subtree may not exist in CAS,
		// check subtree's root digest exist in CAS first.
		// If so, we can assume subtree data exist in CAS.
		// Otherwise, we need to upload subtree data to CAS.
		rootDS := digest.NewStore()
		data, ok := ds.Get(st.d)
		if !ok {
			clog.Warningf(ctx, "no tree root digest in store? %s", st.d)
			st.err = fmt.Errorf("no tree root digst in store")
			return
		}
		rootDS.Set(data)
		ds.Delete(st.d)
		missings, err := b.reapiclient.Missing(ctx, []digest.Digest{st.d})
		fullUpload := func(ctx context.Context) {
			started := time.Now()
			// upload non-root digest first
			n, err := b.reapiclient.UploadAll(ctx, ds)
			if err != nil {
				clog.Warningf(ctx, "failed to upload subtree data %s: %v", dir, err)
				st.mu.Lock()
				defer st.mu.Unlock()
				st.err = err
				return
			}
			// upload root digest last.
			m, err := b.reapiclient.UploadAll(ctx, rootDS)
			clog.Infof(ctx, "upload subtree data %s %d+%d in %s: %v", dir, n, m, time.Since(started), err)
			st.mu.Lock()
			defer st.mu.Unlock()
			st.err = err
		}
		if err == nil && len(missings) == 0 {
			clog.Infof(ctx, "subtree data is ready %s %s", dir, st.d)
			go func() {
				// make sure all data are uploaded in background.
				ctx := context.WithoutCancel(ctx)
				fullUpload(ctx)
			}()
			return
		}
		clog.Infof(ctx, "need to upload subtree data %s (missings=%d): %v", dir, len(missings), err)
		fullUpload(ctx)
	})
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.err
}
