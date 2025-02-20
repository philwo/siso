// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.chromium.org/infra/build/siso/build"
	"go.chromium.org/infra/build/siso/hashfs"
)

// Test schedule for abs path correctly. b/354792946
func TestBuild_Local_AbsPath(t *testing.T) {
	ctx := context.Background()
	topdir := t.TempDir()

	dir := filepath.Join(topdir, "src")
	err := os.MkdirAll(dir, 0755)
	if err != nil {
		t.Fatal(err)
	}
	buildercacheDir := filepath.Join(topdir, "buildercache")
	buildercacheDir, err = filepath.Abs(buildercacheDir)
	if err != nil {
		t.Fatal(err)
	}
	err = os.MkdirAll(buildercacheDir, 0755)
	if err != nil {
		t.Fatal(err)
	}
	buildercacheDir = filepath.ToSlash(buildercacheDir)

	ninja := func(t *testing.T) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile: ".siso_fs_state",
		})
		defer cleanup()
		return runNinja(ctx, "build.ninja", graph, opt, nil, runNinjaOpts{})
	}

	writeFile := func(t *testing.T, fname, content string) {
		t.Helper()
		err := os.MkdirAll(filepath.Dir(fname), 0755)
		if err != nil {
			t.Fatal(err)
		}
		err = os.WriteFile(fname, []byte(content), 0644)
		if err != nil {
			t.Fatal(err)
		}
	}

	writeFile(t, filepath.Join(dir, "build/config/siso/main.star"), `
load("@builtin//encoding.star", "json")
load("@builtin//struct.star", "module")

def __stamp(ctx, cmd):
    ctx.actions.write(cmd.outputs[0])
    ctx.actions.exit(exit_status = 0)

__handlers = {
    "stamp": __stamp,
}

def init(ctx):
    step_config = {
        "rules": [
            {
                "name": "simple/stamp",
                "action": "stamp",
                "handler": "stamp",
                "replace": True,
            },
        ],
    }
    return module(
        "config",
        step_config = json.encode(step_config),
        filegroups = {},
        handlers = __handlers,
    )
`)
	writeFile(t, filepath.Join(dir, "out/siso/build.ninja"), fmt.Sprintf(`
rule stamp
  command = touch ${out}

build gen/foo.inputdeps.stamp: stamp %s/foo.in

build all: phony gen/foo.inputdeps.stamp

build build.ninja: phony
`,
		// need to escape ":" (eps. on windows)
		strings.ReplaceAll(buildercacheDir, ":", "$:")))

	writeFile(t, filepath.Join(buildercacheDir, "foo.in"), "foo input")

	stats, err := ninja(t)
	if err != nil {
		t.Errorf("ninja %v; want nil err", err)
	}
	if stats.NoExec != 1 || stats.Done != stats.Total {
		t.Errorf("noexec=%d done=%d total=%d; want noexec=1 done=total: %#v", stats.NoExec, stats.Done, stats.Total, stats)
	}
}
