// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package msvcutil_test

import (
	"bytes"
	"testing"

	"github.com/google/go-cmp/cmp"

	"infra/build/siso/toolsupport/msvcutil"
)

func TestParseShowIncludes(t *testing.T) {
	inputs, out := msvcutil.ParseShowIncludes([]byte("Note: including file: ../../base/features.h\r\n" +
		"Note: including file:  ../../base/base_export.h\r\n" +
		"Note: including file:  ../../base/feature_list.h\r\n" +
		"Note: including file:   ../../buildtools/third_party/libc++/trunk/include\\atomic\r\n" +
		"In file included from ../../base/version.cc\r\n" +
		"In file included from ../../base/feature_list.h\r\n" +
		"fatal error: 'functional' not found\r\n" +
		"#include <functional>\r\n" +
		"         ^~~~~~~~~~~~\r\n"))
	wantInputs := []string{
		"../../base/features.h",
		"../../base/base_export.h",
		"../../base/feature_list.h",
		`../../buildtools/third_party/libc++/trunk/include\atomic`,
	}
	if diff := cmp.Diff(wantInputs, inputs); diff != "" {
		t.Errorf("msvcutil.ParseShowIncludes(...); inputs diff -want +got:\n%s", diff)
	}
	wantOut := []byte(`In file included from ../../base/version.cc
In file included from ../../base/feature_list.h
fatal error: 'functional' not found
#include <functional>
         ^~~~~~~~~~~~
`)
	wantOut = bytes.ReplaceAll(wantOut, []byte("\n"), []byte("\r\n"))
	if !bytes.Equal(wantOut, out) {
		t.Errorf("msvcutil.ParseShowIncludes(...) got:\n%q\nwant:\n%q", out, wantOut)
	}
}
