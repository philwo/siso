// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"context"
	"testing"

	"infra/build/siso/build"
	"infra/build/siso/hashfs"
)

func TestBuild_MultiOut(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	setupFiles(t, dir, t.Name(), nil)
	opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{})
	defer cleanup()

	b, err := build.New(ctx, graph, opt)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	err = b.Build(ctx, "build", "all")
	if err != nil {
		t.Fatalf(`b.Build(ctx, "build", "all")=%v; want nil err`, err)
	}

	stats := b.Stats()
	t.Logf("err %v; %#v", err, stats)
	if stats.Done != stats.Total {
		t.Errorf("stats.Done=%d Total=%d", stats.Done, stats.Total)
	}
}
