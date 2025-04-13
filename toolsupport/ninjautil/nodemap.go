// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjautil

import (
	"bytes"
	"context"
	"hash/maphash"
	"sync/atomic"

	"github.com/golang/glog"
)

const nodeMapArraySize = 1 << 25

// It is originally designed by
// https://github.com/pcc/ninja/commit/05cddede820a641a1d7793d7f90423e0cf63c3b3.

// nodeMap is a concurrent node map that can lookup/insert a node by key.
// Node are allocated by the slabs of localNodeMaps per goroutine.
type nodeMap struct {
	seed  maphash.Seed
	nodes [nodeMapArraySize]atomic.Pointer[Node]
	n     atomic.Int64 // number of Node inserted.
}

func newNodeMap() *nodeMap {
	return &nodeMap{
		seed: maphash.MakeSeed(),
	}
}

// localNodeMap is a node map for a chunk/goroutine,
// for allocation without lock.
type localNodeMap struct {
	nm    *nodeMap
	nodes slab[Node]
}

func (nm *nodeMap) localNodeMap(n int) *localNodeMap {
	lm := &localNodeMap{
		nm:    nm,
		nodes: newSlab[Node](max(n, 8)),
	}
	return lm
}

// node gets *Node for key.
func (lm *localNodeMap) node(key []byte) *Node {
	h := maphash.Bytes(lm.nm.seed, key)
	n := lm.nm.node(h, key, lm)
	return n
}

// new is used by nodeMap.node.
func (lm *localNodeMap) new(key []byte) *Node {
	n := lm.nodes.new()
	n.path = string(key)
	return n
}

// node is used by localNodeMap.node to return *Node for key.
func (nm *nodeMap) node(h uint64, key []byte, lm *localNodeMap) *Node {
	slot := &nm.nodes[h&(nodeMapArraySize-1)]
	var newHead *Node
	var search *Node
	for {
		head := slot.Load()
		search = head
		for search != nil {
			if bytes.Equal([]byte(search.path), key) {
				if newHead != nil {
					lm.nodes.unnew()
				}
				return search
			}
			search = search.next
		}
		if newHead == nil {
			newHead = lm.new(key)
		}
		newHead.next = head
		if slot.CompareAndSwap(head, newHead) {
			nm.n.Add(1)
			return newHead
		}
	}
}

// lookup returns *Node for key if exists.
func (nm *nodeMap) lookup(key string) (*Node, bool) {
	h := maphash.Bytes(nm.seed, []byte(key))
	node := nm.nodes[h&(nodeMapArraySize-1)].Load()
	for node != nil {
		if node.path == key {
			return node, true
		}
		node = node.next
	}
	return nil, false
}

// freeze freezes bigMap as []*Node, and assign sequence id in *Node.
func (nm *nodeMap) freeze(ctx context.Context) []*Node {
	nodes := make([]*Node, 0, nm.n.Load()+1)
	nodes = append(nodes, nil) // 0: invalid target.
	id := 1
	glog.Infof("freeze bigmap")
	maxDepth := 0
	for i := range nodeMapArraySize {
		n := nm.nodes[i].Load()
		depth := 0
		for n != nil {
			n.id = id
			id++
			nodes = append(nodes, n)
			n = n.next
			depth++
		}
		maxDepth = max(depth, maxDepth)
	}
	glog.Infof("nodes=%d max deps=%d", len(nodes), maxDepth)
	return nodes
}
