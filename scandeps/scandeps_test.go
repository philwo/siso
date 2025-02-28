// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package scandeps

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"go.chromium.org/infra/build/siso/hashfs"
)

func TestScanDeps(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	for fname, content := range map[string]string{
		"base/base.h": `
#include <atomic>

#include "base/extra.h"
#include "base/allocator/allocator_extension.h"
`,
		"base/extra.h": `
#include <map>
#include <string>

#include "base/base_export.h"
`,
		"base/base_export.h": `
`,
		"base/allocator/allocator_extension.h": `
#include "base/base_export.h"
`,
		"apps/apps.h": `
#include <string>
#include "base/base.h"
`,
		"apps/apps.cc": `
#include <unistd.h>

#include <string>
#include "apps/apps.h"
#include "glog/logging.h"
`,
		"third_party/glog/src/glog/logging.h": `
#include <string>
#include <vector>
#include "glog/export.h"
`,
		"third_party/glog/src/glog/export.h": `
`,
		"build/third_party/libc++/trunk/include/__config": "",
		"build/third_party/libc++/trunk/include/atomic":   "",
		"build/third_party/libc++/trunk/include/string":   "",
		"build/third_party/libc++/trunk/include/vector":   "",
		"build/third_party/libc++/trunk/__config_site":    "",
	} {
		fname := filepath.Join(dir, fname)
		err := os.MkdirAll(filepath.Dir(fname), 0755)
		if err != nil {
			t.Fatal(err)
		}
		err = os.WriteFile(fname, []byte(content), 0644)
		if err != nil {
			t.Fatal(err)
		}
	}

	inputDeps := map[string][]string{
		"build/linux/debian_bullseye_amd64-sysroot:headers": {
			"build/linux/debian_bullseye_amd64-sysroot/usr/include/unistd.h",
		},
		"build/third_party/libc++/trunk/include:headers": {
			"build/third_party/libc++/trunk/include/__config",
			"build/third_party/libc++/trunk/include/atomic",
			"build/third_party/libc++/trunk/include/string",
			"build/third_party/libc++/trunk/include/vector",
		},
		"build/third_party/libc++:headers": {
			"build/third_party/libc++/trunk/__config_site",
			"build/third_party/libc++/trunk/include:headers",
		},
	}

	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	scanDeps := New(hashFS, inputDeps)

	req := Request{
		Sources: []string{
			"apps/apps.cc",
		},
		Dirs: []string{
			"",
			"third_party/glog/src",
			"build/third_party/libc++",
			"build/third_party/libc++/trunk/include",
		},
		Sysroots: []string{
			"build/linux/debian_bullseye_amd64-sysroot",
		},
	}

	got, err := scanDeps.Scan(ctx, dir, req)
	if err != nil {
		t.Errorf("scandeps()=%v, %v; want nil err", got, err)
	}

	want := []string{
		"apps",
		"apps/apps.cc",
		"apps/apps.h",
		"base",
		"base/allocator",
		"base/allocator/allocator_extension.h",
		"base/base.h",
		"base/base_export.h",
		"base/extra.h",
		"third_party/glog/src",
		"third_party/glog/src/glog",
		"third_party/glog/src/glog/export.h",
		"third_party/glog/src/glog/logging.h",
	}
	if diff := cmp.Diff(want, got, cmpopts.SortSlices(func(a, b string) bool { return a < b })); diff != "" {
		t.Errorf("scandeps diff -want +got:\n%s", diff)
	}
}

func TestScanDeps_SelfIncludeInCommentAndMacroInclude(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	for fname, content := range map[string]string{
		"third_party/vulkan-deps/vulkan-validation-layers/src/layers/external/vma/vk_mem_alloc.h": `

#ifndef AMD_VULKAN_MEMORY_ALLOCATOR_H
#define AMD_VULKAN_MEMORY_ALLOCATOR_H

/*
    #include "vk_mem_alloc.h"
*/
#if !defined(VMA_CONFIGURATION_USER_INCLUDES_H)
    #include <mutex>
#else
    #include VMA_CONFIGURATION_USER_INCLUDES_H
#endif

#endif
`,
		"apps/apps.cc": `
#include "vma/vk_mem_alloc.h"
`,
	} {
		fname := filepath.Join(dir, fname)
		err := os.MkdirAll(filepath.Dir(fname), 0755)
		if err != nil {
			t.Fatal(err)
		}
		err = os.WriteFile(fname, []byte(content), 0644)
		if err != nil {
			t.Fatal(err)
		}
	}
	inputDeps := map[string][]string{}

	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	scanDeps := New(hashFS, inputDeps)

	req := Request{
		Sources: []string{
			"apps/apps.cc",
		},
		Dirs: []string{
			"",
			"third_party/vulkan-deps/vulkan-validation-layers/src/layers/external",
		},
	}
	got, err := scanDeps.Scan(ctx, dir, req)
	if err != nil {
		t.Errorf("scandeps()=%v, %v; want nil err", got, err)
	}

	want := []string{
		"apps",
		"apps/apps.cc",
		"third_party/vulkan-deps/vulkan-validation-layers/src/layers/external",
		"third_party/vulkan-deps/vulkan-validation-layers/src/layers/external/vma",
		"third_party/vulkan-deps/vulkan-validation-layers/src/layers/external/vma/vk_mem_alloc.h",
	}
	if diff := cmp.Diff(want, got, cmpopts.SortSlices(func(a, b string) bool { return a < b })); diff != "" {
		t.Errorf("scandeps diff -want +got:\n%s", diff)
	}
}

func TestScanDeps_IncludeByDifferentMacroValue(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	for fname, content := range map[string]string{
		"third_party/harfbuzz-ng/src/src/hb-subset.cc": `
#include "hb-ot-post-table.hh"
#include "hb-ot-cff1-table.hh"
`,
		"third_party/harfbuzz-ng/src/src/hb-ot-post-table.hh": `
#ifndef HB_OT_POST_TABLE_HH
#define HB_OT_POST_TABLE_HH

#define HB_STRING_ARRAY_NAME format1_names
#define HB_STRING_ARRAY_LIST "hb-ot-post-macroman.hh"
#include "hb-string-array.hh"
#undef HB_STRING_ARRAY_LIST
#undef HB_STRING_ARRAY_NAME

#endif
`,
		"third_party/harfbuzz-ng/src/src/hb-ot-cff1-table.hh": `
#ifndef HB_OT_CFF1_TABLE_HH
#define HB_OT_CFF1_TABLE_HH

#define HB_STRING_ARRAY_NAME cff1_std_strings
#define HB_STRING_ARRAY_LIST "hb-ot-cff1-std-str.hh"
#include "hb-string-array.hh"
#undef HB_STRING_ARRAY_LIST
#undef HB_STRING_ARRAY_NAME

#endif
`,
		"third_party/harfbuzz-ng/src/src/hb-string-array.hh": `
#ifndef HB_STRING_ARRAY_HH
#if 0 /* Make checks happy. */
#define HB_STRING_ARRAY_HH
#endif

#include HB_STRING_ARRAY_LIST

#endif
`,
		"third_party/harfbuzz-ng/src/src/hb-ot-post-macroman.hh": "",
		"third_party/harfbuzz-ng/src/src/hb-ot-cff1-std-str.hh":  "",
	} {
		fname := filepath.Join(dir, fname)
		err := os.MkdirAll(filepath.Dir(fname), 0755)
		if err != nil {
			t.Fatal(err)
		}
		err = os.WriteFile(fname, []byte(content), 0644)
		if err != nil {
			t.Fatal(err)
		}
	}

	inputDeps := map[string][]string{}
	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	scanDeps := New(hashFS, inputDeps)

	req := Request{
		Sources: []string{
			"third_party/harfbuzz-ng/src/src/hb-subset.cc",
		},
		Dirs: []string{
			"",
			"third_party/harfbuzz-ng/src/src",
		},
	}
	got, err := scanDeps.Scan(ctx, dir, req)
	if err != nil {
		t.Errorf("scandeps()=%v, %v; want nil err", got, err)
	}

	want := []string{
		"third_party/harfbuzz-ng/src/src",
		"third_party/harfbuzz-ng/src/src/hb-subset.cc",
		"third_party/harfbuzz-ng/src/src/hb-ot-post-table.hh",
		"third_party/harfbuzz-ng/src/src/hb-ot-cff1-table.hh",
		"third_party/harfbuzz-ng/src/src/hb-string-array.hh",
		"third_party/harfbuzz-ng/src/src/hb-ot-post-macroman.hh",
		"third_party/harfbuzz-ng/src/src/hb-ot-cff1-std-str.hh",
	}
	if diff := cmp.Diff(want, got, cmpopts.SortSlices(func(a, b string) bool { return a < b })); diff != "" {
		t.Errorf("scandeps diff -want +got:\n%s", diff)
	}
}

func TestScanDeps_Framework(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	for fname, content := range map[string]string{
		"app/app.mm": `
#import <Foo/Bar.h>
`,
		"out/siso/Foo.framework/Headers/Bar.h": `
// Bar.h
`,
	} {
		fname := filepath.Join(dir, fname)
		err := os.MkdirAll(filepath.Dir(fname), 0755)
		if err != nil {
			t.Fatal(err)
		}
		err = os.WriteFile(fname, []byte(content), 0644)
		if err != nil {
			t.Fatal(err)
		}
	}
	inputDeps := map[string][]string{}

	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	scanDeps := New(hashFS, inputDeps)

	req := Request{
		Sources: []string{
			"app/app.mm",
		},
		Dirs: []string{},
		Frameworks: []string{
			"out/siso",
		},
		Sysroots: []string{},
	}
	got, err := scanDeps.Scan(ctx, dir, req)
	if err != nil {
		t.Errorf("scandeps()=%v, %v; want nil err", got, err)
	}

	want := []string{
		"app",
		"app/app.mm",
		"out/siso",
		"out/siso/Foo.framework/Headers",
		"out/siso/Foo.framework/Headers/Bar.h",
	}
	if diff := cmp.Diff(want, got, cmpopts.SortSlices(func(a, b string) bool { return a < b })); diff != "" {
		t.Errorf("scandeps diff -want +got:\n%s", diff)
	}
}

func TestScanDeps_AbsPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skipf("need to check on darwin only for swift generated header, and fails on windows in handling abs path?")
		return
	}
	ctx := context.Background()
	dir := t.TempDir()

	for fname, content := range map[string]string{
		"app/app.mm": `
#include "popup_swift.h"
`,
		"ios/popup_swift_bridge.h": `
#include "ios/ios_string.h"
`,
		"ios/ios_string.h": `
// ios_string.h
`,
		"out/siso/gen/popup_swift.h": `
// generated by swiftc.py
` + fmt.Sprintf(`#import %q
`, filepath.Join(dir, "ios/popup_swift_bridge.h")),
	} {
		fname := filepath.Join(dir, fname)
		err := os.MkdirAll(filepath.Dir(fname), 0755)
		if err != nil {
			t.Fatal(err)
		}
		err = os.WriteFile(fname, []byte(content), 0644)
		if err != nil {
			t.Fatal(err)
		}
	}
	inputDeps := map[string][]string{}

	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	scanDeps := New(hashFS, inputDeps)

	req := Request{
		Sources: []string{
			"app/app.mm",
		},
		Dirs: []string{
			"",
			"out/siso/gen",
		},
		Sysroots: []string{},
	}
	got, err := scanDeps.Scan(ctx, dir, req)
	if err != nil {
		t.Errorf("scandeps()=%v, %v; want nil err", got, err)
	}

	want := []string{
		".",
		"app",
		"app/app.mm",
		"ios",
		"ios/ios_string.h",
		"ios/popup_swift_bridge.h",
		"out/siso/gen",
		"out/siso/gen/popup_swift.h",
	}
	if diff := cmp.Diff(want, got, cmpopts.SortSlices(func(a, b string) bool { return a < b })); diff != "" {
		t.Errorf("scandeps diff -want +got:\n%s", diff)
	}
}

func TestScanDeps_SymlinkDir(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	for fname, content := range map[string]string{
		"x/logging.cc": `
#include "base/logging.h"
`,
		"src/base/logging.h": `
#ifndef BASE_LOGGING_H_
#define BASE_LOGGING_H_

#include <stddef.h>

#endif
`,
	} {
		fname := filepath.Join(dir, fname)
		err := os.MkdirAll(filepath.Dir(fname), 0755)
		if err != nil {
			t.Fatal(err)
		}
		err = os.WriteFile(fname, []byte(content), 0644)
		if err != nil {
			t.Fatal(err)
		}
	}
	err := os.Symlink("../x", filepath.Join(dir, "src/symlink_to_code"))
	if err != nil {
		t.Fatal(err)
	}

	inputDeps := map[string][]string{
		"build/linux/debian_bullseye_amd64-sysroot:headers": {
			"build/linux/debian_bullseye_amd64-sysroot/usr/include/unistd.h",
		},
		"build/third_party/libc++/trunk/include:headers": {
			"build/third_party/libc++/trunk/include/__config",
			"build/third_party/libc++/trunk/include/atomic",
			"build/third_party/libc++/trunk/include/string",
			"build/third_party/libc++/trunk/include/vector",
		},
		"build/third_party/libc++:headers": {
			"build/third_party/libc++/trunk/__config_site",
			"build/third_party/libc++/trunk/include:headers",
		},
	}

	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	scanDeps := New(hashFS, inputDeps)

	req := Request{
		Sources: []string{
			"symlink_to_code/logging.cc",
		},
		Dirs: []string{
			"",
			"build/third_party/libc++",
			"build/third_party/libc++/trunk/include",
		},
		Sysroots: []string{
			"build/linux/debian_bullseye_amd64-sysroot",
		},
	}

	got, err := scanDeps.Scan(ctx, filepath.Join(dir, "src"), req)
	if err != nil {
		t.Errorf("scandeps()=%v, %v; want nil err", got, err)
	}

	want := []string{
		"base",
		"base/logging.h",
		"symlink_to_code",
		"symlink_to_code/logging.cc",
	}
	if diff := cmp.Diff(want, got, cmpopts.SortSlices(func(a, b string) bool { return a < b })); diff != "" {
		t.Errorf("scandeps diff -want +got:\n%s", diff)
	}
}
