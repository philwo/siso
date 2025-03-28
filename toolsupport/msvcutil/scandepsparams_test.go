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
	for _, tc := range []struct {
		name string
		args []string
		env  []string
		want ScanDepsParams
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
			want: ScanDepsParams{
				Sources: []string{
					"compat/msvcrt/snprintf.h",
					"../../base/base64.cc",
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
		{
			name: "clang-cl-sanitize-ignorelist",
			// https://source.chromium.org/chromium/chromium/src/+/cfd9db71a659703304bc2a1af5ecf09d3a354b7d:build/config/sanitizers/BUILD.gn;l=312
			args: []string{
				`..\..\third_party\llvm-build\Release+Asserts\bin\clang-cl.exe`,
				"/c",
				"../../base/base64.cc",
				"/Foobj/base/base/base64.obj",
				"/nologo",
				"/showIncludes:user",
				"/winsysroot../../third_party/depot_tools/win_toolchain/vs_files/27370823e7",
				"-fsanitize-ignorelist=../../tools/cfi/ignores.txt",
			},
			want: ScanDepsParams{
				Sources: []string{
					"../../base/base64.cc",
				},
				Files: []string{
					"../../tools/cfi/ignores.txt",
				},
				Sysroots: []string{
					"../../third_party/llvm-build/Release+Asserts",
					"../../third_party/depot_tools/win_toolchain/vs_files/27370823e7",
				},
				Defines: map[string]string{},
			},
		},
		{
			name: "clang-cl-profile-use",
			// https://source.chromium.org/chromium/chromium/src/+/cfd9db71a659703304bc2a1af5ecf09d3a354b7d:build/config/sanitizers/BUILD.gn;l=341
			args: []string{
				`..\..\third_party\llvm-build\Release+Asserts\bin\clang-cl.exe`,
				"/c",
				"../../base/base64.cc",
				"/Foobj/base/base/base64.obj",
				"/nologo",
				"/showIncludes:user",
				"/winsysroot../../third_party/depot_tools/win_toolchain/vs_files/27370823e7",
				"-fprofile-use=../../chrome/build/pgo_profiles/chrome-win-main-1711928897-084d26c5015f903804b549b12d02ae8f183b9b65-b852f373c4dd312c572a9f1c95892c4a12f81e13.profdata",
			},
			want: ScanDepsParams{
				Sources: []string{
					"../../base/base64.cc",
				},
				Files: []string{
					"../../chrome/build/pgo_profiles/chrome-win-main-1711928897-084d26c5015f903804b549b12d02ae8f183b9b65-b852f373c4dd312c572a9f1c95892c4a12f81e13.profdata",
				},
				Sysroots: []string{
					"../../third_party/llvm-build/Release+Asserts",
					"../../third_party/depot_tools/win_toolchain/vs_files/27370823e7",
				},
				Defines: map[string]string{},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractScanDepsParams(ctx, tc.args, tc.env)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("ExtractScanDepsParams(ctx, %q, %q): diff -want +got:\n%s", tc.args, tc.env, diff)
			}
		})
	}

}
