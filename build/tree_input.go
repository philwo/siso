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

func (b *Builder) treeInput(ctx context.Context, dir, labelSuffix string) (merkletree.TreeEntry, error) {
	if b.reapiclient == nil {
		return merkletree.TreeEntry{}, errors.New("reapi is not configured")
	}
	m := b.graph.InputDeps(ctx)
	files, ok := m[dir+labelSuffix]
	if !ok {
		return merkletree.TreeEntry{}, errors.New("not in input_deps")
	}
	st := &subtree{}
	v, _ := b.trees.LoadOrStore(dir, st)
	st = v.(*subtree)
	st.init(ctx, b, dir, files)
	return merkletree.TreeEntry{
		Name:   dir,
		Digest: st.d,
	}, nil
}

type subtree struct {
	once sync.Once
	d    digest.Digest
}

func (st *subtree) init(ctx context.Context, b *Builder, dir string, files []string) {
	st.once.Do(func() {
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
		ds := digest.NewStore()
		mt := merkletree.New(ds)
		for _, ent := range ents {
			err := mt.Set(ent)
			if err != nil {
				clog.Warningf(ctx, "failed to set %v: %v", ent, err)
			}
		}
		st.d, err = mt.Build(ctx)
		if err != nil {
			clog.Warningf(ctx, "failed to build subtree %s: %v", dir, err)
		}
		// now subtree's digest is ready to use, but
		// file's digests in subtree may not exist in CAS,

		n, err := b.reapiclient.UploadAll(ctx, ds)
		if err != nil {
			clog.Warningf(ctx, "failed to upload subtree data %s: %v", dir, err)
		} else {
			clog.Infof(ctx, "upload subtree data %s %d", dir, n)
		}
	})
}
