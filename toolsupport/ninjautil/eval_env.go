// Copyright 2023 The Chromium Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjautil

import (
	"bytes"
	"fmt"
	"strings"
)

// Env is an interface for an environment for evaluation.
type Env interface {
	// Look up the variable in the environment.
	Lookup(string) string
}

type tokenStrType int

const (
	tokenStrLiteral = iota
	tokenStrVariable
)

type tokenStr struct {
	t tokenStrType
	s []byte
}

// EvalString is an eval string.
// TODO(b/267409605): Add tests for EvalString methods.
type EvalString struct {
	s []tokenStr
}

// Evaluate evaluates the eval string in the env.
func (e EvalString) Evaluate(env Env) []byte {
	if len(e.s) == 1 && e.s[0].t == tokenStrLiteral {
		return e.s[0].s
	}
	var buf bytes.Buffer
	for _, t := range e.s {
		switch t.t {
		case tokenStrLiteral:
			buf.Write(t.s)
		case tokenStrVariable:
			buf.WriteString(env.Lookup(string(t.s)))
		}
	}
	return buf.Bytes()
}

// RawString returns a raw string of eval string.
func (e EvalString) RawString() string {
	var sb strings.Builder
	for _, t := range e.s {
		switch t.t {
		case tokenStrLiteral:
			sb.Write(t.s)
		case tokenStrVariable:
			sb.Write([]byte("${"))
			sb.Write(t.s)
			sb.Write([]byte("}"))
		}
	}
	return sb.String()
}

func (e EvalString) empty() bool {
	return len(e.s) == 0
}

func (e *EvalString) addLiteral(p []byte) {
	e.s = append(e.s, tokenStr{t: tokenStrLiteral, s: p})
}

func (e *EvalString) addVar(p []byte) {
	e.s = append(e.s, tokenStr{t: tokenStrVariable, s: p})
}

// String returns parsed eval string.
func (e EvalString) String() string {
	var sb strings.Builder
	literal := false
	for _, t := range e.s {
		switch t.t {
		case tokenStrLiteral:
			if !literal {
				sb.WriteString("[")
			}
			sb.Write(t.s)
			literal = true
		case tokenStrVariable:
			if literal {
				sb.WriteString("]")
				literal = false
			}
			fmt.Fprintf(&sb, "[$%s]", t.s)
		}
	}
	if literal {
		sb.WriteString("]")
	}
	return sb.String()
}

// Rule represents a build rule.
// TODO(b/267409605): Add tests for Rule methods.
type Rule struct {
	name     string
	bindings map[string]EvalString
}

func newRule(name string) *Rule {
	return &Rule{
		name:     name,
		bindings: make(map[string]EvalString),
	}
}

// Name returns rule's name.
func (r *Rule) Name() string { return r.name }

// AddBinding adds bindings to the rule.
func (r *Rule) AddBinding(key string, val EvalString) {
	r.bindings[key] = val
}

// Binding returns binding in the rule.
func (r *Rule) Binding(key string) (EvalString, bool) {
	v, ok := r.bindings[key]
	return v, ok
}

func (r *Rule) hasBinding(key string) bool {
	_, ok := r.bindings[key]
	return ok
}

// BindingEnv is a binding env.
// TODO(b/267409605): Add tests for BindingEnv methods.
type BindingEnv struct {
	rules    map[string]*Rule
	bindings map[string]string
	parent   *BindingEnv
}

func newBindingEnv(parent *BindingEnv) *BindingEnv {
	return &BindingEnv{
		rules:    make(map[string]*Rule),
		bindings: make(map[string]string),
		parent:   parent,
	}
}

// Lookup looks up key in the binding env.
func (b *BindingEnv) Lookup(key string) string {
	v, ok := b.bindings[key]
	if ok {
		return v
	}
	if b.parent != nil {
		return b.parent.Lookup(key)
	}
	return ""
}

func (b *BindingEnv) addRule(rule *Rule) {
	b.rules[rule.Name()] = rule
}

// LookupRule looks up rules in the binding env.
func (b *BindingEnv) LookupRule(ruleName string) (*Rule, bool) {
	r, ok := b.rules[ruleName]
	if ok {
		return r, true
	}
	if b.parent != nil {
		return b.parent.LookupRule(ruleName)
	}
	return nil, false
}

// LookupRuleCurrentScope looks up rules in the current scope in the binding env.
func (b *BindingEnv) LookupRuleCurrentScope(ruleName string) (*Rule, bool) {
	r, ok := b.rules[ruleName]
	return r, ok
}

// AddBinding adds binding to the binding env.
func (b *BindingEnv) AddBinding(key, val string) {
	b.bindings[key] = val
}

// LookupWithFallback looks up binding env and fallback to v in env if not found.
func (b *BindingEnv) LookupWithFallback(key string, v EvalString, env Env) string {
	val, ok := b.bindings[key]
	if ok {
		return val
	}
	if !v.empty() {
		return string(v.Evaluate(env))
	}
	if b.parent != nil {
		return b.parent.Lookup(key)
	}
	return ""
}
