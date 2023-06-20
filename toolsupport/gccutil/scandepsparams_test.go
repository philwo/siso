// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package gccutil

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestScanDepsParams(t *testing.T) {
	ctx := context.Background()
	type result struct {
		Files    []string
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
			name: "clang++",
			args: []string{
				"../../third_party/llvm-build/Release+Asserts/bin/clang++",
				"-MMD",
				"-MF",
				"obj/base/base/base64.o.d",
				"-DDCHECK_ALWAYS_ON=1",
				`-DCR_CLANG_REVISION="llvmorg-17-init-10134-g3da83fba-1"`,
				"-I../..",
				"-Igen",
				"-D__DATE_=",
				"-isystem",
				"../../buildtools/third_party/libc++/trunk/include",
				"-isystem../../buildtools/third_party/libc++abi/trunk/include",
				"--sysroot=../../build/linux/debian_bullseye_amd64-sysroot",
				"-c",
				"../../base/base64.cc",
				"-o",
				"obj/base/base/base64.o",
			},
			want: result{
				Files: []string{
					"../../base/base64.cc",
				},
				Dirs: []string{
					"../..",
					"gen",
					"../../buildtools/third_party/libc++/trunk/include",
					"../../buildtools/third_party/libc++abi/trunk/include",
				},
				Sysroots: []string{
					"../../third_party/llvm-build/Release+Asserts",
					"../../build/linux/debian_bullseye_amd64-sysroot",
				},
				Defines: map[string]string{
					"CR_CLANG_REVISION": `"llvmorg-17-init-10134-g3da83fba-1"`,
				},
			},
		},
		{
			name: "nacl-gcc",
			args: []string{
				"../../native_client/toolchain/linux_x86/nacl_x86_glibc/bin/x86_64-nacl-gcc",
				"-MMD",
				"-MF",
				"obj/base/base/base64.o.d",
				"-DDCHECK_ALWAYS_ON=1",
				"-DNACL_TC_REV=73e5a44d837e54d335b4c618e1dd5d2028947a67",
				"-I../..",
				"-Igen",
				"-D__DATE_=",
				"-c",
				"../../base/base64.cc",
				"-o",
				"obj/base/base/base64.o",
			},
			want: result{
				Files: []string{
					"../../base/base64.cc",
				},
				Dirs: []string{
					"../..",
					"gen",
				},
				Sysroots: []string{
					"../../native_client/toolchain/linux_x86/nacl_x86_glibc",
				},
				Defines: map[string]string{},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got result
			got.Files, got.Dirs, got.Sysroots, got.Defines, got.Err = ScanDepsParams(ctx, tc.args, tc.env)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("ScanDepsParams(ctx, %q, %q): diff -want +got:\n%s", tc.args, tc.env, diff)
			}
		})
	}

}
