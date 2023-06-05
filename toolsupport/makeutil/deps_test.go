// Copyright 2023 The Chromium Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package makeutil

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestParseDeps(t *testing.T) {
	for _, tc := range []struct {
		name     string
		depsfile []byte
		want     []string
	}{
		{
			name:     "simple",
			depsfile: []byte("foo.o:\tbar baz qux"),
			want: []string{
				"bar",
				"baz",
				"qux",
			},
		},
		{
			name:     "spaceinname",
			depsfile: []byte(`foo\ bar.o: baz\ qux`),
			want: []string{
				"baz qux",
			},
		},
		{
			name:     "newlinewhitespaces",
			depsfile: []byte("foo.o :\tbar\\\n\tbaz\\\r\n  qux"),
			want: []string{
				"bar",
				"baz",
				"qux",
			},
		},
		{
			name:     "backslashes",
			depsfile: []byte("foo\\bar.o: baz\\qux\\\n  quux\\corge"),
			want: []string{
				`baz\qux`,
				`quux\corge`,
			},
		},
		{
			name: "rust-multi",
			depsfile: []byte(`clang_x64_for_rust_host_build_tools/obj/third_party/rust/unicode_ident/v1/lib/libunicode_ident-unicode_ident-1.rlib: ../../third_party/rust/unicode_ident/v1/crate/src/lib.rs ../../third_party/rust/unicode_ident/v1/crate/src/tables.rs

../../third_party/rust/unicode_ident/v1/crate/src/lib.rs:
../../third_party/rust/unicode_ident/v1/crate/src/tables.rs:
`),
			want: []string{
				"../../third_party/rust/unicode_ident/v1/crate/src/lib.rs",
				"../../third_party/rust/unicode_ident/v1/crate/src/tables.rs",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseDeps(tc.depsfile)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("ParseDeps(%q) -want +got:\n%s", tc.depsfile, diff)
			}
		})
	}
}
