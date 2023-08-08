// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjautil

import "fmt"

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
	bindings.addRule(phonyRule)
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

func (s *State) addEdge(rule *Rule) *Edge {
	edge := &Edge{
		rule: rule,
		pool: defaultPool,
		env:  s.bindings,
	}
	s.edges = append(s.edges, edge)
	return edge
}

// Node returns a node.
func (s *State) Node(path []byte) *Node {
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
	n := s.Node(path)
	edge.inputs = append(edge.inputs, n)
	n.addOutEdge(edge)
}

func (s *State) addOut(edge *Edge, path []byte) bool {
	// Nodes can only have one incoming edge
	n := s.Node(path)
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

// AddBinding adds bindings.
func (s *State) AddBinding(name, value string) {
	s.bindings.AddBinding(name, value)
}

// LookupBinding looks up binding.
func (s *State) LookupBinding(name string) string {
	return s.bindings.Lookup(name)
}

// Filenames returns files parsed by the parser (e.g. build.ninja and its subninja etc.)
func (s *State) Filenames() []string {
	return s.filenames
}
