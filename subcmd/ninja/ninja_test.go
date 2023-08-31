// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"infra/build/siso/build"
	"infra/build/siso/build/buildconfig"
	"infra/build/siso/build/ninjabuild"
	"infra/build/siso/hashfs"
	"infra/build/siso/toolsupport/ninjautil"
)

func setupFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for k, v := range files {
		fname := filepath.Join(dir, k)
		dir := filepath.Dir(fname)
		err := os.MkdirAll(dir, 0755)
		if err != nil {
			t.Fatal(err)
		}
		err = os.WriteFile(fname, []byte(v), 0644)
		if err != nil {
			t.Fatal(err)
		}
	}
}

func setupBuild(ctx context.Context, t *testing.T, dir string) (build.Options, build.Graph) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		err := os.Chdir(wd)
		if err != nil {
			t.Fatal(err)
		}
	})
	err = os.MkdirAll(filepath.Join(dir, "out/siso"), 0755)
	if err != nil {
		t.Fatal(err)
	}
	err = os.Chdir(filepath.Join(dir, "out/siso"))
	if err != nil {
		t.Fatal(err)
	}
	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	config, err := buildconfig.New(ctx, "@config//main.star", map[string]string{}, map[string]fs.FS{
		"config":           os.DirFS(filepath.Join(dir, "build/config/siso")),
		"config_overrides": os.DirFS(filepath.Join(dir, ".siso_remote")),
	})
	if err != nil {
		t.Fatal(err)
	}
	path := build.NewPath(dir, "out/siso")
	depsLog, err := ninjautil.NewDepsLog(ctx, ".siso_deps")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		err := depsLog.Close()
		if err != nil {
			t.Fatal(err)
		}
	})
	graph, err := ninjabuild.NewGraph(ctx, "build.ninja", config, path, hashFS, depsLog)
	if err != nil {
		t.Fatal(err)
	}
	cachestore, err := build.NewLocalCache(".siso_cache")
	if err != nil {
		t.Logf("no local cache enabled: %v", err)
	}
	cache, err := build.NewCache(ctx, build.CacheOptions{
		Store: cachestore,
	})
	if err != nil {
		t.Fatal(err)
	}
	opt := build.Options{
		Path:   path,
		HashFS: hashFS,
		Cache:  cache,
	}
	return opt, graph
}

func openDepsLog(ctx context.Context, t *testing.T, dir string) *ninjautil.DepsLog {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		err := os.Chdir(wd)
		if err != nil {
			t.Fatal(err)
		}
	})
	err = os.Chdir(filepath.Join(dir, "out/siso"))
	if err != nil {
		t.Fatal(err)
	}
	depsLog, err := ninjautil.NewDepsLog(ctx, ".siso_deps")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		err := depsLog.Close()
		if err != nil {
			t.Fatal(err)
		}
	})
	return depsLog
}

func TestBuild_SwallowFaiulres(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	setupFiles(t, dir, map[string]string{
		"build/config/siso/main.star": `
load("@builtin//struct.star", "module")
def init(ctx):
  return module(
    "config",
    step_config = "{}",
    filegroups = {},
    handlers = {},
  )
`,
		"out/siso/build.ninja": `
rule fail
  command = fail
build out1: fail
build out2: fail
build out3: fail
build all: phony out1 out2 out3
`})
	opt, graph := setupBuild(ctx, t, dir)
	opt.UnitTest = true
	opt.FailuresAllowed = 3

	b, err := build.New(ctx, graph, opt)
	if err != nil {
		t.Fatal(err)
	}
	err = b.Build(ctx, "build", "all")
	if err == nil {
		t.Fatal(`b.Build(ctx, "build", "all")=nil, want err`)
	}

	stats := b.Stats()
	t.Logf("err %v; %#v", err, stats)
	if got, want := stats.Fail, 3; got != want {
		t.Errorf("stas.Fail=%d; want=%d", got, want)
	}
}

func TestBuild_SwallowFaiulresLimit(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	setupFiles(t, dir, map[string]string{
		"build/config/siso/main.star": `
load("@builtin//struct.star", "module")
def init(ctx):
  return module(
    "config",
    step_config = "{}",
    filegroups = {},
    handlers = {},
  )
`,
		"out/siso/build.ninja": `
rule fail
  command = fail
build out1: fail
build out2: fail
build out3: fail
build out4: fail
build out5: fail
build out6: fail
build all: phony out1 out2 out3 out4 out5 out6
`})
	opt, graph := setupBuild(ctx, t, dir)
	opt.UnitTest = true
	opt.FailuresAllowed = 11

	b, err := build.New(ctx, graph, opt)
	if err != nil {
		t.Fatal(err)
	}
	err = b.Build(ctx, "build", "all")
	if err == nil {
		t.Fatal(`b.Build(ctx, "build", "all")=nil, want err`)
	}

	stats := b.Stats()
	t.Logf("err %v; %#v", err, stats)
	if got, want := stats.Fail, 6; got != want {
		t.Errorf("stas.Fail=%d; want=%d", got, want)
	}
}

func TestBuild_KeepGoing(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	setupFiles(t, dir, map[string]string{
		"build/config/siso/main.star": `
load("@builtin//struct.star", "module")
def init(ctx):
  return module(
    "config",
    step_config = "{}",
    filegroups = {},
    handlers = {},
  )
`,
		"success.py": `
import sys
with open(sys.argv[1], "w") as f:
  f.write("")
`,
		"out/siso/build.ninja": `
rule success
  command = python3 ../../success.py ${out}
rule fail
  command = fail
build out1: fail
build out2: fail
build out3: success
build out4: success
build out5: success
build out6: success
build out7: success out1
build out8: success out2
build out9: success out3
build out10: success out4
build out11: success out5
build out12: success out6
build all: phony out1 out2 out3 out4 out5 out6 out7 out8 out9 out10 out11 out12
`})
	opt, graph := setupBuild(ctx, t, dir)
	opt.UnitTest = true
	opt.FailuresAllowed = 11
	var metricsBuffer bytes.Buffer
	opt.MetricsJSONWriter = &metricsBuffer

	b, err := build.New(ctx, graph, opt)
	if err != nil {
		t.Fatal(err)
	}
	err = b.Build(ctx, "build", "all")
	if err == nil {
		t.Fatal(`b.Build(ctx, "build", "all")=nil, want err`)
	}

	stats := b.Stats()
	t.Logf("err %v; %#v", err, stats)
	if got, want := stats.Fail, 2; got != want {
		t.Errorf("stats.Fail=%d; want=%d", got, want)
	}
	if got, want := stats.Done, 8; got != want {
		t.Errorf("stats.Done=%d; want=%d", got, want)
	}
	dec := json.NewDecoder(bytes.NewReader(metricsBuffer.Bytes()))
	for dec.More() {
		var m build.StepMetric
		err := dec.Decode(&m)
		if err != nil {
			t.Errorf("decode %v", err)
		}
		if m.StepID == "" {
			continue
		}
		switch filepath.Base(m.Output) {
		case "out1", "out2":
			if !m.Err {
				t.Errorf("%s err=%t; want true", m.Output, m.Err)
			}
		case "out3", "out4", "out5", "out6", "out9", "out10", "out11", "out12":
			if m.Err {
				t.Errorf("%s err=%t; want false", m.Output, m.Err)
			}
		default:
			t.Errorf("unexpected output %q", m.Output)
		}
	}
}

func TestParseFlagsFully(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      []string
		want      []string
		wantDebug debugMode
	}{
		{
			name: "simple",
			args: []string{"-C", "out/siso"},
			want: nil,
		},
		{
			name: "target",
			args: []string{"-C", "out/siso", "-project", "rbe-chrome-untrusted", "chrome"},
			want: []string{"chrome"},
		},
		{
			name: "after-flag",
			args: []string{"-C", "out/siso", "-project", "rbe-chrome-untrusted", "chrome", "-d", "explain"},
			want: []string{"chrome"},
			wantDebug: debugMode{
				Explain: true,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &ninjaCmdRun{}
			c.init()
			err := c.Flags.Parse(tc.args)
			if err != nil {
				t.Fatalf("flag parse %v; want nil err", err)
			}
			err = parseFlagsFully(&c.Flags)
			if err != nil {
				t.Fatalf("flag parse fully %v; want nil err", err)
			}
			if diff := cmp.Diff(tc.want, c.Flags.Args()); diff != "" {
				t.Errorf("args diff -want +got:\n%s", diff)
			}
			if diff := cmp.Diff(tc.wantDebug, c.debugMode); diff != "" {
				t.Errorf("debugMode diff -want +got:\n%s", diff)
			}
		})
	}
}
