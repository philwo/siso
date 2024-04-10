// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package msvcutil

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestScanDepsParams(t *testing.T) {
	ctx := context.Background()
	type result struct {
		Files    []string
		Includes []string
		Dirs     []string
		Sysroots []string
		Defines  map[string]string
		Err      error
	}
	for _, tc := range []struct {
		name string
		args []string
		env  []string
		want result
	}{
		{
			name: "clang-cl.exe",
			args: []string{
				`..\..\third_party\llvm-build\Release+Asserts\bin\clang-cl.exe`,
				"/c",
				"../../base/base64.cc",
				"/Foobj/base/base/base64.obj",
				"/nologo",
				"/showIncludes:user",
				"/winsysroot../../third_party/depot_tools/win_toolchain/vs_files/27370823e7",
				"-DDCHECK_ALWAYS_ON=1",
				`-DCR_CLANG_REVISION="llvmorg-17-init-10134-g3da83fba-1"`,
				"-I../..",
				"-Igen",
				"-I../../buildtools/third_party/libc++",
				"-I../../buildtools/third_party/libc++/trunk/include",
				"-D__DATE_=",
				"/FIcompat/msvcrt/snprintf.h",
				"/Fdobj/base/base64_cc.pdb",
			},
			want: result{
				Files: []string{
					"../../base/base64.cc",
				},
				Includes: []string{
					"compat/msvcrt/snprintf.h",
				},
				Dirs: []string{
					"../..",
					"gen",
					"../../buildtools/third_party/libc++",
					"../../buildtools/third_party/libc++/trunk/include",
				},
				Sysroots: []string{
					"../../third_party/llvm-build/Release+Asserts",
					"../../third_party/depot_tools/win_toolchain/vs_files/27370823e7",
				},
				Defines: map[string]string{
					"CR_CLANG_REVISION": `"llvmorg-17-init-10134-g3da83fba-1"`,
				},
			},
		},
		// TODO: add more tests?
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got result
			got.Files, got.Includes, got.Dirs, got.Sysroots, got.Defines, got.Err = ScanDepsParams(ctx, tc.args, tc.env)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("ScanDepsParams(ctx, %q, %q): diff -want +got:\n%s", tc.args, tc.env, diff)
			}
		})
	}

}
