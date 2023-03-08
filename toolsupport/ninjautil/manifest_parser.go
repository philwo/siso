// Copyright 2023 The Chromium Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjautil

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"time"

	log "github.com/golang/glog"

	"infra/build/siso/o11y/clog"
)

// ManifestParser parses Ninja manifests. (i.e. .ninja files)
type ManifestParser struct {
	state *State
	env   *BindingEnv
	lexer *lexer
}

// NewManifestParser creates a new manifest parser.
func NewManifestParser(state *State) *ManifestParser {
	return &ManifestParser{
		state: state,
		env:   state.bindings,
	}
}

// Load loads the Ninja manifest given an fname.
func (p *ManifestParser) Load(ctx context.Context, fname string) error {
	buf, err := os.ReadFile(fname)
	if err != nil {
		return err
	}
	return p.parse(ctx, &lexer{fname: fname, buf: buf})
}

func (p *ManifestParser) parse(ctx context.Context, l *lexer) error {
	p.state.filenames = append(p.state.filenames, l.fname)
	p.lexer = l
	for {
		tok, err := l.Next()
		if err != nil {
			return err
		}
		if log.V(5) {
			clog.Infof(ctx, "token %T %q", tok, tok)
		}
		switch tok := tok.(type) {
		case tokenPool:
			err := p.parsePool()
			if err != nil {
				return err
			}
		case tokenBuild:
			err := p.parseEdge()
			if err != nil {
				return err
			}
		case tokenRule:
			err := p.parseRule()
			if err != nil {
				return err
			}
		case tokenDefault:
			err := p.parseDefault()
			if err != nil {
				return err
			}
		case tokenIdent:
			l.Back()
			name, letval, err := p.parseLet()
			if err != nil {
				return err
			}
			val := string(letval.Evaluate(p.env))
			// TODO(ukai): check ninja version if name == "ninja_required_version"
			p.env.AddBinding(name, val)
		case tokenInclude:
			err := p.parseFileInclude(ctx, false)
			if err != nil {
				return err
			}
		case tokenSubninja:
			err := p.parseFileInclude(ctx, true)
			if err != nil {
				return err
			}
		case tokenEOF:
			return nil
		case tokenNewline:
		default:
			return l.errorf("unexpected %T %q", tok, tok)
		}
	}
}

func (p *ManifestParser) parsePool() error {
	name, err := p.lexer.Ident()
	if err != nil {
		return err
	}
	err = p.expectToken(tokenNewline{})
	if err != nil {
		return err
	}
	_, ok := p.state.LookupPool(name.String())
	if ok {
		return p.lexer.errorf("duplicate pool %q", name)
	}
	depth := -1
	for p.lexer.Peek(tokenIndent{}) {
		key, value, err := p.parseLet()
		if err != nil {
			return err
		}
		switch key {
		case "depth":
			ds := string(value.Evaluate(p.env))
			depth, err = strconv.Atoi(ds)
			if err != nil || depth < 0 {
				return p.lexer.errorf("invalid pool depth %s: %v", value, err)
			}
		default:
			return p.lexer.errorf("unexpected variable %q", key)
		}
	}
	if depth < 0 {
		return p.lexer.errorf("expected 'depth =' line")
	}
	p.state.addPool(newPool(name.String(), depth))
	return nil
}

func (p *ManifestParser) parseEdge() error {
	var outs []EvalString
	out, err := p.lexer.Path()
	if err != nil {
		return err
	}
	for !out.empty() {
		outs = append(outs, out)
		out, err = p.lexer.Path()
		if err != nil {
			return err
		}
	}
	implicitOuts := 0
	if p.lexer.Peek(tokenPipe{}) {
		for {
			out, err := p.lexer.Path()
			if err != nil {
				return err
			}
			if out.empty() {
				break
			}
			outs = append(outs, out)
			implicitOuts++
		}
	}
	if len(outs) == 0 {
		return p.lexer.errorf("expected path")
	}
	err = p.expectToken(tokenColon{})
	if err != nil {
		return err
	}
	ruleName, err := p.lexer.Ident()
	if err != nil {
		return p.lexer.errorf("expected build command name: %v", err)
	}
	rule, ok := p.env.LookupRule(ruleName.String())
	if !ok {
		return p.lexer.errorf("unknown build rule %s", ruleName)
	}
	var ins []EvalString
	for {
		in, err := p.lexer.Path()
		if err != nil {
			return err
		}
		if in.empty() {
			break
		}
		ins = append(ins, in)
	}

	implicit := 0
	if p.lexer.Peek(tokenPipe{}) {
		for {
			in, err := p.lexer.Path()
			if err != nil {
				return err
			}
			if in.empty() {
				break
			}
			ins = append(ins, in)
			implicit++
		}
	}
	orderOnly := 0
	if p.lexer.Peek(tokenPipe2{}) {
		for {
			in, err := p.lexer.Path()
			if err != nil {
				return err
			}
			if in.empty() {
				break
			}
			ins = append(ins, in)
			orderOnly++
		}
	}

	err = p.expectToken(tokenNewline{})
	if err != nil {
		return err
	}
	hasIndent := p.lexer.Peek(tokenIndent{})
	var env *BindingEnv = p.env
	if hasIndent {
		env = newBindingEnv(env)
	}
	for hasIndent {
		key, val, err := p.parseLet()
		if err != nil {
			return err
		}
		env.AddBinding(key, string(val.Evaluate(env)))
		hasIndent = p.lexer.Peek(tokenIndent{})
	}

	edge := p.state.addEdge(rule)
	edge.env = env

	poolName := edge.Binding("pool")
	if poolName != "" {
		pool, ok := p.state.LookupPool(poolName)
		if !ok {
			return p.lexer.errorf("unknown pool name %q", poolName)
		}
		edge.pool = pool
	}
	edge.outputs = make([]*Node, 0, len(outs))
	for _, out := range outs {
		path := bytes.TrimPrefix(out.Evaluate(env), []byte("./"))
		if !p.state.addOut(edge, path) {
			return p.lexer.errorf("multiple rules generate %s", path)
		}
	}
	edge.implicitOuts = implicitOuts
	edge.inputs = make([]*Node, 0, len(ins))
	for _, in := range ins {
		path := bytes.TrimPrefix(in.Evaluate(env), []byte("./"))
		p.state.addIn(edge, path)
	}
	edge.implicitDeps = implicit
	edge.orderOnlyDeps = orderOnly
	return nil
}

func (p *ManifestParser) parseRule() error {
	name, err := p.lexer.Ident()
	if err != nil {
		return p.lexer.errorf("expected rule name")
	}
	err = p.expectToken(tokenNewline{})
	if err != nil {
		return err
	}
	_, found := p.env.LookupRuleCurrentScope(name.String())
	if found {
		return p.lexer.errorf("duplicate rule %q", name)
	}
	rule := newRule(name.String())
	for p.lexer.Peek(tokenIndent{}) {
		key, value, err := p.parseLet()
		if err != nil {
			return err
		}
		// TODO(ukai): check reserved binding?
		rule.AddBinding(key, value)
	}
	if rule.hasBinding("rspfile") != rule.hasBinding("rspfile_content") {
		return p.lexer.errorf("rspfile and rspfile_content need to be both specified")
	}
	if !rule.hasBinding("command") {
		return p.lexer.errorf("expected 'command =' line")
	}
	p.env.addRule(rule)
	return nil
}

func (p *ManifestParser) parseDefault() error {
	v, err := p.lexer.Path()
	if err != nil {
		return err
	}
	if v.empty() {
		return p.lexer.errorf("expected target name")
	}
	for {
		path := string(v.Evaluate(p.env))
		path = filepath.Clean(path)
		err := p.state.addDefault(path)
		if err != nil {
			return p.lexer.errorf("%v", err)
		}
		v, err = p.lexer.Path()
		if err != nil {
			return err
		}
		if v.empty() {
			break
		}
	}
	return p.expectToken(tokenNewline{})
}

func (p *ManifestParser) parseLet() (string, EvalString, error) {
	name, err := p.lexer.Ident()
	if err != nil {
		return "", EvalString{}, p.lexer.errorf("expected vairable name: %v", err)
	}
	err = p.expectToken(tokenEq{})
	if err != nil {
		return "", EvalString{}, err
	}
	value, err := p.lexer.VarValue()
	if err != nil {
		return "", EvalString{}, err
	}
	return name.String(), value, nil
}

func (p *ManifestParser) parseFileInclude(ctx context.Context, newScope bool) error {
	s, err := p.lexer.Path()
	if err != nil {
		return err
	}
	path := string(s.Evaluate(p.env))

	op := "include"
	subparser := NewManifestParser(p.state)
	if newScope {
		subparser.env = newBindingEnv(p.env)
		op = "subninja"
	} else {
		subparser.env = p.env
	}

	select {
	case <-ctx.Done():
		clog.Warningf(ctx, "interrupted in ninja build parser: %v", ctx.Err())
		return fmt.Errorf("interrupted in ninja builder parser: %w", ctx.Err())
	default:
	}
	start := time.Now()
	err = subparser.Load(ctx, path)
	if err != nil {
		clog.Errorf(ctx, "Failed %s %s %s: %v", op, path, time.Since(start), err)

		return err
	}
	clog.Infof(ctx, "%s %s %s", op, path, time.Since(start))

	err = p.expectToken(tokenNewline{})
	if err != nil {
		return err
	}
	return nil
}

func (p *ManifestParser) expectToken(want token) error {
	got, err := p.lexer.Next()
	if err != nil {
		return err
	}
	if reflect.TypeOf(got) != reflect.TypeOf(want) {
		return p.lexer.errorf("expected %T, got %T (%s)", want, got, got)
	}
	return nil
}
