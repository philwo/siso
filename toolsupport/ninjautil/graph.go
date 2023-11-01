// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjautil

import (
	"fmt"
	"strings"
)

// Node represents a node (target file) in build graph.
type Node struct {
	path   string
	inEdge *Edge // the edge that generates the file for this node.
	outs   []*Edge
}

func (n *Node) String() string { return n.path }

// Path is the path of the node.
func (n *Node) Path() string { return n.path }

func (n *Node) addOutEdge(e *Edge) {
	n.outs = append(n.outs, e)
}

// OutEdges returns out-edges of the node.
func (n *Node) OutEdges() []*Edge {
	return n.outs
}

func (n *Node) hasInEdge() bool {
	return n.inEdge != nil
}

// InEdge returns in-edge of the node.
func (n *Node) InEdge() (*Edge, bool) {
	e := n.inEdge
	return e, e != nil
}

// Edge represents an edge (action) in build graph.
// TODO(b/267409605): Add tests for Edge methods.
type Edge struct {
	rule *Rule

	// For now, setting pool doesn't affect Siso builds as all because Siso utilizes remote computation resources.
	pool    *Pool
	env     *BindingEnv
	inputs  []*Node
	outputs []*Node

	// https://ninja-build.org/manual.html#ref_dependencies
	implicitDeps  int
	orderOnlyDeps int
	// https://ninja-build.org/manual.html#ref_outputs
	implicitOuts int
}

type edgeEnv struct {
	edge        *Edge
	shellEscape bool
	lookups     []string
	recursive   bool
}

func (e *edgeEnv) Lookup(key string) string {
	sep := " "
	// Handle special in/out keys.
	// https://ninja-build.org/manual.html#ref_rule
	switch key {
	case "in_newline":
		sep = "\n"
		fallthrough
	case "in":
		n := len(e.edge.inputs) - e.edge.implicitDeps - e.edge.orderOnlyDeps
		return e.pathList(e.edge.inputs[:n], sep)
	case "out":
		n := len(e.edge.outputs) - e.edge.implicitOuts
		return e.pathList(e.edge.outputs[:n], sep)
	}
	if e.recursive {
		for _, s := range e.lookups {
			if s == key {
				// TODO(b/271218091): better to return error property rather than panic,
				// as it doesn't recover on package boundary. It won't happen in chromium source,
				// so no need to fix it now, but better to fix it sometime.
				panic(fmt.Errorf("cycle in rule variables %q", e.lookups))
			}
		}
	}
	val, ok := e.edge.rule.Binding(key)
	if ok {
		e.lookups = append(e.lookups, key)
	}
	e.recursive = true
	return e.edge.env.LookupWithFallback(key, val, e)
}

func (e *edgeEnv) pathList(paths []*Node, sep string) string {
	var s []string
	for _, n := range paths {
		p := n.path
		if e.shellEscape {
			p = shellEscape(p)
		}
		s = append(s, p)
	}
	return strings.Join(s, sep)
}

// Binding returns binding value in the edge.
func (e *Edge) Binding(name string) string {
	env := edgeEnv{edge: e, shellEscape: true}
	return env.Lookup(name)
}

// BindingBool returns true if binding is defined in the edge.
func (e *Edge) BindingBool(name string) bool {
	return e.Binding(name) != ""
}

// UnescapedBinding returns binding value without shell escape.
func (e *Edge) UnescapedBinding(name string) string {
	env := edgeEnv{edge: e, shellEscape: false}
	return env.Lookup(name)
}

// RawBinding returns raw eval string of binding value in the edge.
func (e *Edge) RawBinding(name string) string {
	env := edgeEnv{edge: e, shellEscape: true}
	sep := " "
	// Handle special in/out keys.
	// https://ninja-build.org/manual.html#ref_rule
	switch name {
	case "in_newline":
		sep = "\n"
		fallthrough
	case "in":
		n := len(env.edge.inputs) - env.edge.implicitDeps - env.edge.orderOnlyDeps
		return env.pathList(env.edge.inputs[:n], sep)
	case "out":
		n := len(env.edge.outputs) - env.edge.implicitOuts
		return env.pathList(env.edge.outputs[:n], sep)
	}
	val, ok := env.edge.rule.Binding(name)
	if !ok {
		return ""
	}
	return val.RawString()
}

// Rule returns a rule of the edge.
func (e *Edge) Rule() *Rule {
	return e.rule
}

// Pool returns a pool associated to the edge.
func (e *Edge) Pool() *Pool {
	return e.pool
}

// Inputs returns input nodes of the edge.
func (e *Edge) Inputs() []*Node {
	return e.inputs
}

// Ins returns explicit input nodes (for $in) of the edge.
func (e *Edge) Ins() []*Node {
	n := len(e.inputs) - e.implicitDeps - e.orderOnlyDeps
	return e.inputs[:n]
}

// TriggerInputs returns inputs nodes of the edge that would trigger
// the edge command. i.e. not including order_only inputs.
func (e *Edge) TriggerInputs() []*Node {
	return e.inputs[:len(e.inputs)-e.orderOnlyDeps]
}

// Outputs returns output nodes of the edge.
func (e *Edge) Outputs() []*Node {
	return e.outputs
}

// IsPhony returns true iff phony edge.
func (e *Edge) IsPhony() bool {
	return e.rule == phonyRule
}
