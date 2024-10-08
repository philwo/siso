// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"infra/build/siso/build"
	"infra/build/siso/hashfs"
)

func TestBuild_CheckDeps(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	ninja := func(t *testing.T, w io.Writer) (build.Stats, error) {
		t.Helper()
		build.SetExperimentForTest("check-deps")
		defer func() {
			build.SetExperimentForTest("")
		}()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile: ".siso_fs_state",
		})
		defer cleanup()
		opt.OutputLogWriter = w
		return runNinja(ctx, "build.ninja", graph, opt, nil, runNinjaOpts{})
	}

	setupFiles(t, dir, t.Name(), nil)
	var sisoOutput bytes.Buffer
	stats, err := ninja(t, &sisoOutput)
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}
	if stats.Done != stats.Total || stats.Total != 5 {
		t.Errorf("done=%d total=%d; want done=total=5", stats.Done, stats.Total)
	}

	t.Logf("siso_output:\n%s", sisoOutput.String())

	want := `deps error: deps inputs have no dependencies from "./obj/foo.o" to ["bar.h"]`
	if !strings.Contains(sisoOutput.String(), want) {
		t.Errorf("unexpected siso_output\ngot:\n%s\nwant contains\n%s", sisoOutput.String(), want)
	}
}
