// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjautil

import (
	"context"
	"fmt"
	"os"
	"sync"

	"golang.org/x/sync/errgroup"
)

// fileState stores per-file states.
type fileState struct {
	mu        sync.Mutex
	edges     []*Edge
	defaults  []*Node
	filenames []string
	subninjas []string
}

// addDefaults adds nodes for default target.
func (fs *fileState) addDefaults(nodes []*Node) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.defaults = append(fs.defaults, nodes...)
}

// addSubninja adds filenames for subninja.
func (fs *fileState) addSubninja(fname string) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.subninjas = append(fs.subninjas, fname)
}

// fileParser is a parser of a ninja manifest file.
type fileParser struct {
	state *State
	scope *fileScope

	fname  string
	buf    []byte
	full   chunk // for accumulated numbers from chunks
	chunks []chunk

	fileState fileState

	// allocations
	ruleArena    arena[rule]
	edgeArena    arena[Edge]
	poolArena    arena[Pool]
	bindingArena arena[binding]
}

// parseFile parses a file of fname.
func (p *fileParser) parseFile(ctx context.Context, fname string) error {
	p.fname = fname
	p.fileState.filenames = append(p.fileState.filenames, fname)
	var err error
	p.buf, err = p.readFile(fname)
	if err != nil {
		return err
	}
	err = p.parseContent(ctx)
	if err != nil {
		return fmt.Errorf("%s: %w", fname, err)
	}
	return nil
}

// readFile reads a file of fname in parallel.
func (p *fileParser) readFile(fname string) ([]byte, error) {
	return os.ReadFile(fname)
}

// parseContent parses ninja file.
func (p *fileParser) parseContent(ctx context.Context) error {
	p.chunks = splitIntoChunks(p.buf)

	err := p.parseChunks()
	if err != nil {
		return err
	}

	p.alloc()

	err = p.setup(ctx)
	if err != nil {
		return err
	}

	err = p.buildGraph()
	if err != nil {
		return err
	}

	err = p.finalize()
	if err != nil {
		return err
	}
	return nil
}

// parseChunks parses chunks into statements and counts for allocs concurrently.
func (p *fileParser) parseChunks() error {
	var eg errgroup.Group
	for i := range p.chunks {
		eg.Go(func() error {
			return p.chunks[i].parseChunk()
		})
	}
	return eg.Wait()
}

// alloc allocates arenas.
func (p *fileParser) alloc() {
	p.full.start = 0
	p.full.end = len(p.buf)
	pos := 0
	for i := range p.chunks {
		ch := &p.chunks[i]
		p.full.nvar += ch.nvar
		p.full.nrule += ch.nrule
		p.full.nrulevar += ch.nrulevar
		p.full.nbuild += ch.nbuild
		p.full.nbuildvar += ch.nbuildvar
		p.full.npool += ch.npool
		p.full.npoolvar += ch.npoolvar
		p.full.ndefault += ch.ndefault
		p.full.ninclude += ch.ninclude
		p.full.nsubninja += ch.nsubninja
		p.full.ncomment += ch.ncomment
		for j := range ch.statements {
			ch.statements[j].pos = pos
			pos++
		}
	}

	p.ruleArena.reserve(p.full.nrule)
	p.edgeArena.reserve(p.full.nbuild)
	p.poolArena.reserve(p.full.npool)
	p.bindingArena.reserve(p.full.nrulevar + p.full.nbuildvar)

	p.scope.rules = newRuleMap(p.full.nrule)
	p.scope.bindings = newShardBindings(p.full.nvar)

	for i := range p.chunks {
		ch := &p.chunks[i]
		ch.nodemap = p.state.nodeMap.localNodeMap(ch.nbuild) // estimates # of nodes

		ch.ruleArena = p.ruleArena.chunk(ch.nrule)
		ch.edgeArena = p.edgeArena.chunk(ch.nbuild)
		ch.poolArena = p.poolArena.chunk(ch.npool)
		ch.bindingArena = p.bindingArena.chunk(ch.nrulevar + ch.nbuildvar)

		// estimates number of outputs/inputs/validations
		ch.outPaths = make([]evalString, 0, (ch.end-ch.start)/256)
		ch.inPaths = make([]evalString, 0, (ch.end-ch.start)/256)
		ch.validationPaths = make([]evalString, 0, 4)
		ch.edgePathSlab = newSlab[*Node](ch.nbuild)
	}
}

// setup prepares binding scopes (var, pool, rule).
func (p *fileParser) setup(ctx context.Context) error {
	var eg errgroup.Group
	if p.full.ninclude > 0 {
		eg.SetLimit(1)
	}
	for i := range p.chunks {
		ch := &p.chunks[i]
		eg.Go(func() error {
			return ch.setupInChunk(ctx, p.state, p.scope)
		})
		// adjust statement positions if there is any include in any chunk.
		if p.full.ninclude > 0 {
			pos := ch.statements[len(ch.statements)-1].pos
			if i < len(p.chunks) && p.chunks[i+1].statements[0].pos < pos {
				nch := &p.chunks[i+1]
				for j := range nch.statements {
					nch.statements[j].pos = pos + 1
					pos++
				}
			}
		}
	}
	return eg.Wait()
}

// buildGraph parses build statements and rule bindings concurrently.
func (p *fileParser) buildGraph() error {
	var eg errgroup.Group
	for i := range p.chunks {
		ch := &p.chunks[i]
		eg.Go(func() error {
			return ch.buildGraphInChunk(p.state, &p.fileState, p.scope)
		})
		for j := range ch.includes {
			inc := ch.includes[j]
			for k := range inc {
				ich := &inc[k]
				eg.Go(func() error {
					return ich.buildGraphInChunk(p.state, &p.fileState, p.scope)
				})
			}
		}
	}
	return eg.Wait()
}

// finalize finalizes a file parsing.
func (p *fileParser) finalize() error {
	p.fileState.edges = make([]*Edge, 0, p.edgeArena.len())
	for i := range p.edgeArena.used() {
		edge := p.edgeArena.at(i)
		if edge.rule != nil {
			p.fileState.edges = append(p.fileState.edges, edge)
		}
	}
	p.state.mergeFileState(&p.fileState)
	p.chunks = nil
	return nil
}
