// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package scandeps

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"infra/build/siso/hashfs"
)

func TestScanDeps(t *testing.T) {
	t.Skip("TODO(b/282888305) enable this")
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

	hashfs, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	scanDeps := New(hashfs, inputDeps)

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
