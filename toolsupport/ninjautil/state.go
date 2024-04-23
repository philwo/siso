// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjautil

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"infra/build/siso/scandeps"
)

var (
	// The default pool is used when no other pool is specified. It has no limit on the number of concurrent actions.
	defaultPool = newPool("", 0)
	// The console pool allows tasks access to stdin/stdout/stderr, thus is limited to one concurrent action.
	consolePool = newPool("console", 1)
	// The phony rule is used to "alias" targets to another name. We implement this as a "rule" that just takes
	// the specified targets as inputs, produces no outputs, runs no command.
	phonyRule = newRule("phony")
)

// A Pool limits the number of concurrently running actions.
type Pool struct {
	// Name identifies how this pool is referred to from rules or build statements.
	name string
	// Depth is the maximum number of actions allowed to run concurrently.
	depth int
}

// Name returns the pool name.
func (p *Pool) Name() string {
	return p.name
}

// Depth returns the depth of the pool.
func (p *Pool) Depth() int {
	return p.depth
}

func newPool(name string, depth int) *Pool {
	return &Pool{name: name, depth: depth}
}

// State contains all information in a scope about Ninja primitives relevant during execution time,
// such as pools, edges (= actions) and variable bindings.
type State struct {
	// Pools map from pool name to *Pool.
	pools map[string]*Pool
	// Paths map from target name (or pathname used in build's inputs/outputs) to *Node.
	paths map[string]*Node
	// Edges is a list of all actions defined in the Ninja file.
	edges []*Edge
	// Bindings contains all rules and variables defined in this scope.
	bindings *BindingEnv
	// Defaults is a list of default targets, i.e. those built if the user does not specify targets to build.
	defaults []*Node
	// Filenames parsed by the parser (e.g. build.ninja and its subninja etc.)
	filenames []string
}

// NewState creates new state.
func NewState() *State {
	bindings := newBindingEnv(nil)
	return &State{
		pools: map[string]*Pool{
			"":        defaultPool,
			"console": consolePool,
		},
		// Pre-allocate for performance.
		// TODO(ukai): Benchmark this.
		paths:    make(map[string]*Node, 65536),
		bindings: bindings,
	}
}

func (s *State) addPool(pool *Pool) {
	s.pools[pool.name] = pool
}

// LookupPool looks up pool.
func (s *State) LookupPool(poolName string) (*Pool, bool) {
	p, ok := s.pools[poolName]
	return p, ok
}

// Pools returns a map of pools
func (s *State) Pools() map[string]*Pool {
	m := make(map[string]*Pool)
	for k, v := range s.pools {
		m[k] = v
	}
	return m
}

func (s *State) addEdge(rule *rule) *Edge {
	edge := &Edge{
		rule: rule,
		pool: defaultPool,
		env:  s.bindings,
	}
	s.edges = append(s.edges, edge)
	return edge
}

// node returns a node.
func (s *State) node(path []byte) *Node {
	n, ok := s.paths[string(path)]
	if ok {
		return n
	}
	pathStr := string(path)
	n = &Node{path: pathStr}
	s.paths[pathStr] = n
	return n
}

// NumNodes returns a number of nodes.
func (s *State) NumNodes() int {
	return len(s.paths)
}

// LookupNode returns a node.
func (s *State) LookupNode(path string) (*Node, bool) {
	n, ok := s.paths[path]
	return n, ok
}

func (s *State) addIn(edge *Edge, path []byte) {
	n := s.node(path)
	edge.inputs = append(edge.inputs, n)
	n.addOutEdge(edge)
}

func (s *State) addOut(edge *Edge, path []byte) bool {
	// Nodes can only have one incoming edge
	n := s.node(path)
	if n.hasInEdge() {
		return false
	}
	edge.outputs = append(edge.outputs, n)
	n.inEdge = edge
	return true
}

func (s *State) addDefault(path string) error {
	n, ok := s.LookupNode(path)
	if !ok {
		return fmt.Errorf("unknown target %q", path)
	}
	s.defaults = append(s.defaults, n)
	return nil
}

// RootNodes returns root nodes, that are nodes without output actions.
// (Hence can be considered final artifacts, i.e. other nodes will not use root nodes as inputs.)
func (s *State) RootNodes() ([]*Node, error) {
	var roots []*Node
	for _, e := range s.edges {
		for _, out := range e.outputs {
			if len(out.outs) == 0 {
				roots = append(roots, out)
			}
		}
	}
	if len(s.edges) != 0 && len(roots) == 0 {
		return nil, fmt.Errorf("could not determine root nodes of build graph")
	}
	return roots, nil
}

// DefaultNodes returns default nodes.
func (s *State) DefaultNodes() ([]*Node, error) {
	ns := s.defaults
	if len(ns) > 0 {
		return ns, nil
	}
	return s.RootNodes()
}

// Targets returns target nodes for given args.
// If args is empty, returns default nodes.
// If arg has ^-suffix, it is treated as source of target.
// e.g. "foo.cc^" will be "foo.o", which is generated from foo.cc
func (s *State) Targets(args []string) ([]*Node, error) {
	if len(args) == 0 {
		return s.DefaultNodes()
	}
	nodes := make([]*Node, 0, len(args))
	for _, t := range args {
		t := filepath.ToSlash(t)
		var node *Node
		if strings.HasSuffix(t, "^") {
			n, ok := s.hatTarget(t)
			if !ok {
				return nil, fmt.Errorf("unknown target %q", t)
			}
			outs := n.OutEdges()
			if len(outs) == 0 {
				// TODO(b/289309062): deps log first reverse deps node?
				return nil, fmt.Errorf("no outs for %q", t)
			}
			edge := outs[0]
			outputs := edge.Outputs()
			if len(outputs) == 0 {
				return nil, fmt.Errorf("out edge of %q has no output", t)
			}
			node = outputs[0]
		} else {
			n, ok := s.LookupNode(t)
			if !ok {
				return nil, fmt.Errorf("unknown target %q", t)
			}
			node = n
		}
		nodes = append(nodes, node)
	}
	return nodes, nil
}

// Special syntax: "foo.cc^" means "the first output of foo.cc".
// for header file, try to find one of the source file which has
// a direct #include for the header.
// i.e. "foo.h^" will be equivalent with "foo.cc^"
// TODO(b/336185923): document this.
func (s *State) hatTarget(t string) (*Node, bool) {
	ctx := context.Background() // TODO: take from caller.
	t = strings.TrimSuffix(t, "^")
	n, ok := s.LookupNode(t)
	if ok {
		return n, true
	}
	switch filepath.Ext(t) {
	case ".h", ".hxx", ".hpp", ".inc":
	default:
		return nil, false
	}
	// special handling for header file.
	incname := filepath.Base(t)
	dirname := filepath.Dir(t)
	// check the file include the header file.
	// TODO: use hashfs here?
	dirents, err := os.ReadDir(dirname)
	if err != nil {
		return nil, false
	}
	files := make(map[string]bool)
	for _, dirent := range dirents {
		files[dirent.Name()] = true
	}
	tbase := strings.TrimSuffix(t, filepath.Ext(t))

	checkNode := func(fname string) (*Node, bool) {
		buf, err := os.ReadFile(fname)
		if err != nil {
			return nil, false
		}
		includes, _, err := scandeps.CPPScan(ctx, fname, buf)
		if err != nil {
			return nil, false
		}
		for _, inc := range includes {
			// inc should be `"foo.h"` or `<foo.h>`
			switch inc[len(inc)-1] {
			case '"', '>':
				inc = inc[1 : len(inc)-1]
			default:
				continue
			}
			if filepath.Base(inc) == incname {
				n, ok := s.LookupNode(fname)
				if ok {
					return n, true
				}
			}
		}
		return nil, false
	}
	// prefer same stem, test or unittest.
	sourceExts := []string{".cc", "_test.cc", "_unittest.cc", ".c", ".cxx", ".cpp", ".m", ".mm", ".S"}
	for _, ext := range sourceExts {
		tt := tbase + ext
		delete(files, filepath.Base(tt))
		n, ok := checkNode(tt)
		if ok {
			return n, true
		}
	}
	var filenames []string
	for fname := range files {
		filenames = append(filenames, fname)
	}
	sort.Strings(filenames)
	for _, fname := range filenames {
		n, ok := checkNode(filepath.ToSlash(filepath.Join(dirname, fname)))
		if ok {
			return n, true
		}
	}
	return nil, false
}

// PhonyNodes returns phony's output nodes.
func (s *State) PhonyNodes() []*Node {
	var phony []*Node
	for _, e := range s.edges {
		if e.IsPhony() {
			phony = append(phony, e.Outputs()...)
		}
	}
	return phony
}

// AddBinding adds bindings.
func (s *State) AddBinding(name, value string) {
	s.bindings.addBinding(name, value)
}

// LookupBinding looks up binding.
func (s *State) LookupBinding(name string) string {
	return s.bindings.Lookup(name)
}

// Filenames returns files parsed by the parser (e.g. build.ninja and its subninja etc.)
func (s *State) Filenames() []string {
	return s.filenames
}
