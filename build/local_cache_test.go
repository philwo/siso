// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"testing"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/protobuf/proto"
)

func TestActionResultCache(t *testing.T) {
	ctx := context.Background()
	cache, err := NewLocalCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	digest := makeDigest("newly added")
	val, err := cache.GetActionResult(ctx, digest)
	if err == nil || val != nil {
		t.Errorf("GetActionResult unexpectedly succeeded on an empty cache")
	}

	want := &rpb.ActionResult{StdoutRaw: []byte("a")}

	if err := cache.SetActionResult(ctx, digest, want); err != nil {
		t.Errorf("cache.SetActionResult(%v, %v) = %v; want nil", digest, want, err)
	}

	got, err := cache.GetActionResult(ctx, digest)
	if err != nil || !proto.Equal(got, want) {
		t.Errorf("cache.GetActionResult(%v) = %v, %v; want %v, nil", digest, got, err, want)
	}
}
