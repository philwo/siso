// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package ninjautil provides utilitites for ninja.
package ninjautil

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"

	"golang.org/x/sync/errgroup"
)

// multipleRulesError is an error that multiple rules generates the same output.
type multipleRulesError struct {
	target string
}

func (e multipleRulesError) Error() string {
	return fmt.Sprintf("multiple rules generates %s", e.target)
}

// ManifestParser parses Ninja manifests. (i.e. .ninja files)
type ManifestParser struct {
	// Stores all information found while parsing the .ninja file.
	state *State
	scope *fileScope

	eg *errgroup.Group
	wd string
}

// NewManifestParser creates a new manifest parser.
func NewManifestParser(state *State) *ManifestParser {
	scope := newFileScope(state.scope)
	scope.rules = newRuleMap(1)
	_ = scope.setRule(phonyRule)
	return &ManifestParser{
		state: state,
		scope: scope,
	}
}

var loaderConcurrency = runtime.NumCPU()

// Load loads the Ninja manifest given an fname.
func (p *ManifestParser) Load(ctx context.Context, fname string) error {
	if p.eg == nil {
		p.eg, ctx = errgroup.WithContext(ctx)
	}
	p.eg.Go(func() error {
		return p.loadFile(ctx, fname)
	})
	err := p.eg.Wait()
	if err != nil {
		return err
	}
	p.state.nodes = p.state.nodeMap.freeze()
	for _, edge := range p.state.edges {
		for _, in := range edge.inputs {
			if in.outs == nil {
				in.outs = make([]*Edge, 0, in.nouts.Load())
			}
			in.outs = append(in.outs, edge)
		}
	}
	return nil
}

func (p *ManifestParser) loadFile(ctx context.Context, fname string) error {
	fp := &fileParser{
		parent: p.scope,
		state:  p.state,
	}
	err := fp.parseFile(ctx, filepath.Join(p.wd, fname))
	if err != nil {
		return err
	}
	for _, fname := range fp.fileState.subninjas {
		scope := &fp.scope
		p.eg.Go(func() error {
			subparser := &ManifestParser{
				state: p.state,
				scope: scope,
				eg:    p.eg,
				wd:    p.wd,
			}
			return subparser.loadFile(ctx, fname)
		})
	}
	return nil
}
