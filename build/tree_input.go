// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	log "github.com/golang/glog"

	"infra/build/siso/o11y/clog"
	"infra/build/siso/reapi/digest"
	"infra/build/siso/reapi/merkletree"
)

func (b *Builder) treeInputs(ctx context.Context, labelSuffix string, sysroots, dirs []string) []merkletree.TreeEntry {
	var treeEntries []merkletree.TreeEntry
	for _, dir := range sysroots {
		ti, err := b.treeInput(ctx, dir, labelSuffix)
		if err != nil {
			clog.Warningf(ctx, "treeinput[sysroot] %s: %v", dir, err)
			continue
		}
		treeEntries = append(treeEntries, ti)
	}
	for _, dir := range dirs {
		ti, err := b.treeInput(ctx, dir, labelSuffix)
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

func (b *Builder) treeInput(ctx context.Context, dir, labelSuffix string) (merkletree.TreeEntry, error) {
	if b.reapiclient == nil {
		return merkletree.TreeEntry{}, errors.New("reapi is not configured")
	}
	m := b.graph.InputDeps(ctx)
	files, ok := m[dir+labelSuffix]
	if !ok {
		return merkletree.TreeEntry{}, errors.New("not in input_deps")
	}
	st := &subtree{ready: make(chan struct{})}
	v, loaded := b.trees.LoadOrStore(dir, st)
	if !loaded {
		st.init(ctx, b, dir, files)
	}
	st = v.(*subtree)
	select {
	case <-ctx.Done():
		return merkletree.TreeEntry{}, ctx.Err()
	case <-st.ready:
	}
	st.mu.Lock()
	ds := st.ds
	st.mu.Unlock()
	return merkletree.TreeEntry{
		Name:   dir,
		Digest: st.d,
		Store:  ds,
	}, nil
}

type subtree struct {
	ready chan struct{}
	d     digest.Digest

	mu sync.Mutex
	ds *digest.Store
}

func (st *subtree) init(ctx context.Context, b *Builder, dir string, files []string) {
	files = b.expandInputs(ctx, files)
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
	}
	// keep digest in tree in st.ds
	st.ds = digest.NewStore()
	mt := merkletree.New(st.ds)
	for _, ent := range ents {
		err := mt.Set(ent)
		if err != nil {
			clog.Warningf(ctx, "failed to set %v: %v", ent, err)
		}
	}
	d, err := mt.Build(ctx)
	if err != nil {
		clog.Warningf(ctx, "failed to build subtree %s: %v", dir, err)
	}
	st.d = d
	clog.Infof(ctx, "subtree ready %s: %s", dir, d)
	close(st.ready)
	// now subtree's digest is ready to use, but
	// file's digests in subtree may not exist in CAS,
	// so uploading here, or action may use st.ds for file's digests.

	n, err := b.reapiclient.UploadAll(ctx, st.ds)
	if err != nil {
		clog.Warningf(ctx, "failed to upload subtree data %s: %v", dir, err)
	} else {
		clog.Infof(ctx, "upload subtree data %s %d", dir, n)
	}
	st.mu.Lock()
	// now all file's digests have been uploaded in CAS,
	// so action that uses this subtree doesn't need to check
	// file's digest, so reset st.ds.
	st.ds = nil
	st.mu.Unlock()
}
