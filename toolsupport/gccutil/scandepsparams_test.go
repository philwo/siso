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
	for _, tc := range []struct {
		name string
		args []string
		env  []string
		want ScanDepsParams
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
			want: ScanDepsParams{
				Sources: []string{
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
			want: ScanDepsParams{
				Sources: []string{
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
		{
			name: "clang++_mac",
			args: []string{
				"../../third_party/llvm-build/Release+Asserts/bin/clang++",
				"-MMD",
				"-MF",
				"obj/third_party/abseil-cpp/absl/strings/str_format_internal/arg.o.d",
				"-DDCHECK_ALWAYS_ON=1",
				"-I../..",
				"-Igen",
				"-isysroot",
				"../../build/mac_files/xcode_binaries/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX13.3.sdk",
				"-mmacos-version-min=10.15",
				"-isystem../../buildtools/third_party/libc++/trunk/include",
				"-isystem../../buildtools/third_party/libc++abi/trunk/include",
				"-c",
				"../../third_party/abseil-cpp/absl/strings/internal/str_format/arg.cc",
				"-o",
				"obj/third_party/abseil-cpp/absl/strings/str_format_internal/arg.o",
			},
			want: ScanDepsParams{
				Sources: []string{
					"../../third_party/abseil-cpp/absl/strings/internal/str_format/arg.cc",
				},
				Dirs: []string{
					"../..",
					"gen",
					"../../buildtools/third_party/libc++/trunk/include",
					"../../buildtools/third_party/libc++abi/trunk/include",
				},
				Sysroots: []string{
					"../../third_party/llvm-build/Release+Asserts",
					"../../build/mac_files/xcode_binaries/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX13.3.sdk",
				},
				Defines: map[string]string{},
			},
		},
		{
			name: "clang_ios",
			args: []string{
				"../../third_party/llvm-build/Release+Asserts/bin/clang",
				"-MMD",
				"-MF",
				"obj/ios/third_party/earl_grey2/test_lib/TCXTestCase+GREYTest.o.d",
				"-DDCHECK_ALWAYS_ON=1",
				"-I../..",
				"-Igen",
				"-isysroot",
				"sdk/xcode_links/iPhoneSimulator16.4.sdk",
				"-iframework",
				"sdk/xcode_links/iPhoneSimulator.platform/Developer/Library/Frameworks",
				"-iframework",
				"sdk/xcode_links/iPhoneSimulator16.4.sdk/Developer/Library/Frameworks",
				"-isystem../../third_party/libc++/src/include",
				"-isystem../../third_party/libc++abi/src/include",
				"-c",
				"../../base/test/ios/google_test_runner.mm",
				"-o",
				"obj/base/test/goolge_test_runner/goolge_test_runner.o",
			},
			want: ScanDepsParams{
				Sources: []string{
					"../../base/test/ios/google_test_runner.mm",
				},
				Dirs: []string{
					"../..",
					"gen",
					"../../third_party/libc++/src/include",
					"../../third_party/libc++abi/src/include",
				},
				Frameworks: []string{
					"sdk/xcode_links/iPhoneSimulator.platform/Developer/Library/Frameworks",
					"sdk/xcode_links/iPhoneSimulator16.4.sdk/Developer/Library/Frameworks",
				},
				Sysroots: []string{
					"../../third_party/llvm-build/Release+Asserts",
					"sdk/xcode_links/iPhoneSimulator16.4.sdk",
				},
				Defines: map[string]string{},
			},
		},
		{
			name: "clang-sanitize-ignorelist",
			args: []string{
				"../../third_party/llvm-build/Release+Asserts/bin/clang",
				"-MMD",
				"-MF",
				"obj/build/rust/tests/bindgen_static_fns_test/c_lib/lib.o.d",
				"-fsanitize-ignorelist=../../tools/cfi/ignores.txt",
				"--sysroot=../../build/linux/debian_bullseye_amd64-sysroot",
				"-c",
				"../../build/rust/tests/bindgen_static_fns_test/lib.c",
				"-o",
				"obj/build/rust/tests/bindgen_static_fns_test/c_lib/lib.o",
			},
			want: ScanDepsParams{
				Sources: []string{
					"../../build/rust/tests/bindgen_static_fns_test/lib.c",
				},
				Files: []string{
					"../../tools/cfi/ignores.txt",
				},
				Sysroots: []string{
					"../../third_party/llvm-build/Release+Asserts",
					"../../build/linux/debian_bullseye_amd64-sysroot",
				},
				Defines: map[string]string{},
			},
		},
		{
			name: "clang-profile-use",
			args: []string{
				"../../third_party/llvm-build/Release+Asserts/bin/clang",
				"-MMD",
				"-MF",
				"obj/third_party/nasm/nasm/stdscan.o.d",
				"-fprofile-use=../../chrome/build/pgo_profiles/chrome-linux-main-1711928897-084d26c5015f903804b549b12d02ae8f183b9b65-b852f373c4dd312c572a9f1c95892c4a12f81e13.profdata",
				"--sysroot=../../build/linux/debian_bullseye_amd64-sysroot",
				"-c",
				"../../third_party/nasm/asm/stdscan.c",
				"-o",
				"obj/third_party/nasm/nasm/stdscan.o",
			},
			want: ScanDepsParams{
				Sources: []string{
					"../../third_party/nasm/asm/stdscan.c",
				},
				Files: []string{
					"../../chrome/build/pgo_profiles/chrome-linux-main-1711928897-084d26c5015f903804b549b12d02ae8f183b9b65-b852f373c4dd312c572a9f1c95892c4a12f81e13.profdata",
				},
				Sysroots: []string{
					"../../third_party/llvm-build/Release+Asserts",
					"../../build/linux/debian_bullseye_amd64-sysroot",
				},
				Defines: map[string]string{},
			},
		},
		{
			name: "clang-proflie-sample-use",
			args: []string{
				"../../third_party/llvm-build/Release+Asserts/bin/clang++",
				"-MMD",
				"-MF",
				"android_clang_arm/obj/skia/skia_core_and_effects/SkRecords.o.d",
				"-fprofile-sample-use=../../chrome/android/profiles/afdo.prof",
				"-c",
				"../../third_party/skia/src/core/SkRecords.cpp",
				"-o",
				"android_clang_arm/obj/skia/skia_core_and_effects/SkRecords.o",
			},
			want: ScanDepsParams{
				Sources: []string{
					"../../third_party/skia/src/core/SkRecords.cpp",
				},
				Files: []string{
					"../../chrome/android/profiles/afdo.prof",
				},
				Sysroots: []string{
					"../../third_party/llvm-build/Release+Asserts",
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
