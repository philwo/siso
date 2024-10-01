// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjautil

import (
	"context"
	"hash/maphash"
	"sync/atomic"

	"infra/build/siso/o11y/clog"
)

// bigMap is originally designed by
// https://github.com/pcc/ninja/commit/05cddede820a641a1d7793d7f90423e0cf63c3b3.
// This is the lock free hash table from path to node mapping.

const bigMapArraySize = 1 << 20

type bigMap struct {
	seed  maphash.Seed
	nodes [bigMapArraySize]atomic.Pointer[Node] // *Node
}

func (bm *bigMap) node(key []byte) *Node {
	h := maphash.Bytes(bm.seed, key)
	slot := &bm.nodes[h&(bigMapArraySize-1)]
	var newHead *Node
	var search *Node
	for {
		head := slot.Load()
		search = head
		for search != nil {
			if search.path == string(key) {
				return search
			}
			search = search.next
		}
		if newHead == nil {
			newHead = &Node{path: string(key)}
		}
		newHead.next = head
		if slot.CompareAndSwap(head, newHead) {
			return newHead
		}
	}
}

func (bm *bigMap) lookup(key string) (*Node, bool) {
	h := maphash.Bytes(bm.seed, []byte(key))
	node := bm.nodes[h&(bigMapArraySize-1)].Load()
	for node != nil {
		if node.path == key {
			return node, true
		}
		node = node.next
	}
	return nil, false
}

func (bm *bigMap) freeze(ctx context.Context) ([]*Node, map[string]*Node) {
	nodes := []*Node{nil} // 0: invalid target.
	paths := make(map[string]*Node)
	id := 1
	clog.Infof(ctx, "freeze bigmap")
	maxDepth := 0
	for i := range bigMapArraySize {
		n := bm.nodes[i].Load()
		depth := 0
		for n != nil {
			n.id = id
			id++
			nodes = append(nodes, n)
			paths[n.path] = n
			n = n.next
			depth++
		}
		maxDepth = max(depth, maxDepth)

	}
	clog.Infof(ctx, "nodes=%d max deps=%d", len(nodes), maxDepth)
	return nodes, paths
}
