// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjautil

import (
	"bytes"
	"fmt"
	"hash/maphash"
	"sync/atomic"
)

// rule represents a build rule.
// Further reading: https://ninja-build.org/manual.html#_rules
// TODO(b/267409605): Add tests for Rule methods.
type rule struct {
	next     *rule // for ruleMap
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

// ruleMap is a map rules by its name for fast concurrent updates.
type ruleMap struct {
	seed  maphash.Seed
	rules []atomic.Pointer[rule]
}

// newRuleMap creates new ruleMap for size n.
func newRuleMap(n int) *ruleMap {
	rm := &ruleMap{
		seed: maphash.MakeSeed(),
	}
	// numRulesPerBuckets is the number of rules per buckets.
	// We want to keep it small to avoid contention.
	const numRulesPerBuckets = 8
	// prepare sufficient buckets for size n.
	numBuckets := 1
	for n/numBuckets >= numRulesPerBuckets {
		numBuckets <<= 1
	}
	rm.rules = make([]atomic.Pointer[rule], numBuckets)
	return rm
}

// setRule sets a rule in ruleMap.
func (rm *ruleMap) setRule(r *rule) error {
	if rm == nil || len(rm.rules) == 0 {
		return fmt.Errorf("no rule map")
	}
	var slot *atomic.Pointer[rule]
	if len(rm.rules) == 1 {
		slot = &rm.rules[0]
	} else {
		h := maphash.String(rm.seed, r.name)
		slot = &rm.rules[h&uint64(len(rm.rules)-1)]
	}
	var search *rule
	for {
		head := slot.Load()
		search = head
		for search != nil {
			if search.name == r.name {
				return fmt.Errorf("duplicate rule %q", r.name)
			}
			search = search.next
		}
		r.next = head
		if slot.CompareAndSwap(head, r) {
			return nil
		}
	}
}

// lookupRule looks up rule by its name.
func (rm *ruleMap) lookupRule(name []byte) (*rule, bool) {
	if rm == nil || len(rm.rules) == 0 {
		return nil, false
	}
	var r *rule
	if len(rm.rules) == 1 {
		r = rm.rules[0].Load()
	} else {
		h := maphash.Bytes(rm.seed, name)
		r = rm.rules[h&uint64(len(rm.rules)-1)].Load()
	}
	for r != nil {
		if bytes.Equal([]byte(r.name), name) {
			return r, true
		}
		r = r.next
	}
	return nil, false
}
