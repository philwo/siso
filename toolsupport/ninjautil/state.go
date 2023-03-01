// Copyright 2023 The Chromium Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjautil

// The special phony rule allows aliasing for other targets.
// Further reading: https://ninja-build.org/manual.html#_the_literal_phony_literal_rule
var phonyRule = newRule("phony")

// Pool is a ninja pool.
// Further reading: https://ninja-build.org/manual.html#ref_pool
type Pool struct {
	name  string
	depth int
}

func newPool(name string, depth int) *Pool {
	return &Pool{name: name, depth: depth}
}
