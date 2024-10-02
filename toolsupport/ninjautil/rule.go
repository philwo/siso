// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjautil

// rule represents a build rule.
// Further reading: https://ninja-build.org/manual.html#_rules
// TODO(b/267409605): Add tests for Rule methods.
type rule struct {
	name     string
	bindings map[string]EvalString
}

func newRule(name string) *rule {
	return &rule{
		name:     name,
		bindings: make(map[string]EvalString),
	}
}

// Name returns rule's name.
func (r *rule) Name() string { return r.name }

// addBinding adds bindings to the rule.
func (r *rule) addBinding(key string, val EvalString) {
	r.bindings[key] = val
}

// Binding returns binding in the rule.
func (r *rule) Binding(key string) (EvalString, bool) {
	v, ok := r.bindings[key]
	return v, ok
}

func (r *rule) hasBinding(key string) bool {
	_, ok := r.bindings[key]
	return ok
}

// ruleBinding is a mappings of rule names to rules.
type ruleBinding struct {
	rules  map[string]*rule
	parent *ruleBinding
}

func newRuleBinding(parent *ruleBinding) *ruleBinding {
	return &ruleBinding{
		rules:  make(map[string]*rule),
		parent: parent,
	}
}

func (b *ruleBinding) addRule(rule *rule) {
	b.rules[rule.Name()] = rule
}

// lookupRule looks up rules in the binding env.
func (b *ruleBinding) lookupRule(ruleName string) (*rule, bool) {
	r, ok := b.rules[ruleName]
	if ok {
		return r, true
	}
	if b.parent != nil {
		return b.parent.lookupRule(ruleName)
	}
	return nil, false
}

// lookupRuleCurrentScope looks up rules in the current scope in the binding env.
func (b *ruleBinding) lookupRuleCurrentScope(ruleName string) (*rule, bool) {
	r, ok := b.rules[ruleName]
	return r, ok
}
