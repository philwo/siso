// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package query

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"infra/build/siso/hashfs"
	pb "infra/build/siso/hashfs/proto"
	"infra/build/siso/reapi/digest"
	"infra/build/siso/toolsupport/ninjautil"
)

func TestIDEAnalysis(t *testing.T) {
	ctx := context.Background()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	topDir := t.TempDir()
	err = os.Chdir(topDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		err = os.Chdir(wd)
		if err != nil {
			t.Error(err)
		}
	}()

	dir := filepath.Join(topDir, "out/siso")
	err = os.MkdirAll(dir, 0755)
	if err != nil {
		t.Fatal(err)
	}
	buildNinjaFilename := filepath.Join(dir, "build.ninja")
	setupBuildNinja(t, buildNinjaFilename)

	depsLogFilename := filepath.Join(dir, ".siso_deps")
	setupDepsLog(t, depsLogFilename)

	fsStateFilename := filepath.Join(dir, ".siso_fs_state")
	barH := `// generated file`
	setupFileState(t, topDir, fsStateFilename, map[string]fileState{
		filepath.Join(topDir, "foo/foo.cc"): {
			content: "foo.cc",
		},
		filepath.Join(topDir, "foo/foo.h"): {
			content: "foo.h",
		},
		filepath.Join(topDir, "out/siso/foo"): {
			content:   "foo binary",
			mtime:     3 * time.Millisecond,
			generated: true,
		},
		filepath.Join(topDir, "out/siso/gen/bar/bar.h"): {
			content:   barH,
			mtime:     1 * time.Millisecond,
			generated: true,
		},
		filepath.Join(topDir, "out/siso/obj/foo.o"): {
			content:   "foo obj",
			mtime:     2 * time.Millisecond,
			generated: true,
		},
	})

	c := &ideAnalysisRun{}
	c.init()
	c.dir = "out/siso"

	got, err := c.analysis(ctx, []string{"../../foo/foo.cc^"})
	if err != nil {
		t.Errorf(`analysis(ctx, "../../foo/foo.cc^")=%v, %v; want nil er`, got, err)
	}

	want := IDEAnalysis{
		BuildArtifactRoot: "out/siso",
		Sources: []SourceFile{
			{
				Path:       "../../foo/foo.cc",
				WorkingDir: "out/siso",
				CompilerArguments: []string{
					"clang++",
					"-MMD",
					"-MF",
					"obj/foo.o.d",
					"-Igen",
					"-c",
					"../../foo/foo.cc",
					"-o",
					"obj/foo.o",
				},
				Generated: []GeneratedFile{
					{
						Path:     "gen/bar/bar.h",
						Contents: barH,
					},
				},
				Deps: []string{
					"../../foo/foo.cc",
					"../../foo/foo.h",
					"gen/bar/bar.h",
				},
				Status: Status{
					Code: Ok,
				},
			},
		},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf(`analysis(ctx, "../../foo/foo.cc^") diff -want +got:\n%s`, diff)
	}

}

func setupBuildNinja(t *testing.T, fname string) {
	t.Helper()
	err := os.WriteFile(fname, []byte(`
cflags = -Igen
rule gen
  command = python gen.py ${in} ${out}

rule cxx
  command = clang++ -MMD -MF ${out}.d ${cflags} -c ${in} -o ${out}
  dep = gcc
  depfile = ${out}.d
rule link
  command = clang++ @${rspfile} -o ${out}
  rspfile = ${out}.rsp
  rspfile_content = ${in}

build gen/bar/bar.h: gen ../../bar/bar.in
build obj/foo.o: cxx ../../foo/foo.cc
build foo: link obj/foo.o

`), 0644)
	if err != nil {
		t.Fatal(err)
	}
}

func setupDepsLog(t *testing.T, fname string) {
	t.Helper()
	ctx := context.Background()
	depsLog, err := ninjautil.NewDepsLog(ctx, fname)
	if err != nil {
		t.Fatal(err)
	}
	_, err = depsLog.Record(ctx, "obj/foo.o", time.Now(), []string{"../../foo/foo.cc", "../../foo/foo.h", "gen/bar/bar.h"})
	if err != nil {
		depsLog.Close()
		t.Fatal(err)
	}
	err = depsLog.Close()
	if err != nil {
		t.Fatal(err)
	}
}

type fileState struct {
	content   string
	mtime     time.Duration
	generated bool
}

func setupFileState(t *testing.T, topdir, fname string, files map[string]fileState) {
	t.Helper()
	ctx := context.Background()
	srcMtime := time.Now().Add(-10 * time.Second)
	cmdHash := []byte("someCommandHash")
	state := &pb.State{
		Entries: []*pb.Entry{
			{
				Id: &pb.FileID{
					ModTime: srcMtime.UnixNano(),
				},
				Name: filepath.ToSlash(topdir),
			},
		},
	}
	seen := map[string]bool{
		topdir: true,
	}

	var mkdirAll func(dirname string, mtime time.Time)
	mkdirAll = func(dirname string, mtime time.Time) {
		err := os.MkdirAll(dirname, 0755)
		if err != nil {
			t.Fatal(err)
		}
		if seen[dirname] {
			return
		}
		seen[dirname] = true
		state.Entries = append(state.Entries, &pb.Entry{
			Id: &pb.FileID{
				ModTime: mtime.UnixNano(),
			},
			Name: dirname,
		})
		mkdirAll(filepath.Dir(dirname), mtime)
	}
	writeFile := func(fname, content string, mtime time.Time, generated bool) {
		mkdirAll(filepath.Dir(fname), mtime)
		d := digest.FromBytes(fname, []byte(content))
		ent := &pb.Entry{
			Id: &pb.FileID{
				ModTime: mtime.UnixNano(),
			},
			Name: fname,
			Digest: &pb.Digest{
				Hash:      d.Digest().Hash,
				SizeBytes: d.Digest().SizeBytes,
			},
		}
		if generated {
			ent.CmdHash = cmdHash
			ent.UpdatedTime = mtime.UnixNano()
		}
		state.Entries = append(state.Entries, ent)
		err := os.WriteFile(fname, []byte(content), 0644)
		if err != nil {
			t.Fatal(err)
		}
	}
	for fname, fstate := range files {
		mtime := srcMtime.Add(fstate.mtime)
		writeFile(fname, fstate.content, mtime, fstate.generated)
	}
	sort.Slice(state.Entries, func(i, j int) bool {
		return state.Entries[i].Name < state.Entries[j].Name
	})
	err := hashfs.Save(ctx, fname, state)
	if err != nil {
		t.Fatal(err)
	}
}
