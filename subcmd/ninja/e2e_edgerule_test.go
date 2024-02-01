// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"testing"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/google/go-cmp/cmp"

	"infra/build/siso/build"
	"infra/build/siso/hashfs"
	"infra/build/siso/reapi/reapitest"
)

func TestBuild_EdgeRule(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	ninja := func(t *testing.T, ds dataSource) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:  ".siso_fs_state",
			DataSource: ds,
		})
		defer cleanup()
		opt.REAPIClient = ds.client
		return runNinja(ctx, "build.ninja", graph, opt, nil, runNinjaOpts{})
	}

	setupFiles(t, dir, t.Name(), nil)

	t.Logf("first build")
	// make sure replace / accumulate works
	fakere := &reapitest.Fake{
		ExecuteFunc: func(fakere *reapitest.Fake, action *rpb.Action) (*rpb.ActionResult, error) {
			cmd := &rpb.Command{}
			err := reapitest.FetchProto(ctx, fakere.CAS, action.CommandDigest, cmd)
			if err != nil {
				return nil, err
			}
			switch {
			case cmp.Equal(cmd.Arguments, []string{"python3", "../../tools/clang.py", "-c", "../../foo.cc", "-o", "obj/foo.o"}):
				tree := reapitest.InputTree{CAS: fakere.CAS, Root: action.InputRootDigest}
				fn, err := tree.LookupFileNode(ctx, "foo.cc")
				if err != nil {
					return &rpb.ActionResult{
						ExitCode:  1,
						StderrRaw: []byte(fmt.Sprintf("../../foo.cc: File not found: %v", err)),
					}, nil
				}
				_, err = tree.LookupFileNode(ctx, "out/siso/gen/bar.h")
				if err != nil {
					t.Logf("gen/bar.h not found for foo.cc")
					return &rpb.ActionResult{
						ExitCode:  1,
						StderrRaw: []byte(fmt.Sprintf("gen/bar.h: File not found: %v", err)),
					}, nil
				}
				return &rpb.ActionResult{
					ExitCode: 0,
					OutputFiles: []*rpb.OutputFile{
						{
							Path:   "obj/foo.o",
							Digest: fn.Digest,
						},
					},
				}, nil

			case cmp.Equal(cmd.Arguments, []string{"python3", "../../tools/clang.py", "-c", "gen/bar.cc", "-o", "obj/bar.o"}):
				tree := reapitest.InputTree{CAS: fakere.CAS, Root: action.InputRootDigest}
				fn, err := tree.LookupFileNode(ctx, "out/siso/gen/bar.cc")
				if err != nil {
					return &rpb.ActionResult{
						ExitCode:  1,
						StderrRaw: []byte(fmt.Sprintf("gen/bar.cc: File not found: %v", err)),
					}, nil
				}
				_, err = tree.LookupFileNode(ctx, "out/siso/gen/bar.h")
				if err != nil {
					t.Logf("gen/bar.h not found for gen/bar.cc")
					return &rpb.ActionResult{
						ExitCode:  1,
						StderrRaw: []byte(fmt.Sprintf("gen/bar.h: File not found: %v", err)),
					}, nil
				}
				return &rpb.ActionResult{
					ExitCode: 0,
					OutputFiles: []*rpb.OutputFile{
						{
							Path:   "obj/bar.o",
							Digest: fn.Digest,
						},
					},
				}, nil

			case cmp.Equal(cmd.Arguments, []string{"python3", "../../tools/ar.py", "-c", "obj/bar.a", "obj/bar.o"}):
				tree := reapitest.InputTree{CAS: fakere.CAS, Root: action.InputRootDigest}
				_, err := tree.LookupFileNode(ctx, "out/siso/obj/bar.o")
				if err != nil {
					return &rpb.ActionResult{
						ExitCode:  1,
						StderrRaw: []byte(fmt.Sprintf("obj/bar.o: File not found: %v", err)),
					}, nil
				}
				d, err := reapitest.SetContent(ctx, fakere.CAS, []byte("obj/bar.o\n"))
				if err != nil {
					return &rpb.ActionResult{
						ExitCode:  1,
						StderrRaw: []byte(fmt.Sprintf("obj/bar.a: failed to store %v", err)),
					}, nil
				}
				t.Logf("obj/bar.a => %s", d)
				return &rpb.ActionResult{
					ExitCode: 0,
					OutputFiles: []*rpb.OutputFile{
						{
							Path:   "obj/bar.a",
							Digest: d,
						},
					},
				}, nil

			case cmp.Equal(cmd.Arguments, []string{"python3", "../../tools/link.py", "obj/foo.o", "obj/bar.a", "-o", "obj/foo.so"}):
				var buf bytes.Buffer
				tree := reapitest.InputTree{CAS: fakere.CAS, Root: action.InputRootDigest}
				for _, input := range []string{"obj/foo.o", "obj/bar.a"} {
					fn, err := tree.LookupFileNode(ctx, path.Join("out/siso", input))
					if err != nil {
						return &rpb.ActionResult{
							ExitCode:  1,
							StderrRaw: []byte(fmt.Sprintf("%s: File not found: %v", input, err)),
						}, nil
					}
					fmt.Fprintln(&buf, input)
					data, err := reapitest.Fetch(ctx, fakere.CAS, fn.Digest)
					if err != nil {
						return &rpb.ActionResult{
							ExitCode:  1,
							StderrRaw: []byte(fmt.Sprintf("%s: File content not found: %v", input, err)),
						}, nil
					}
					buf.Write(data)
				}
				// obj/bar.a is thin archive, so it needs obj/bar.o too
				_, err := tree.LookupFileNode(ctx, "out/siso/obj/bar.o")
				if err != nil {
					t.Logf("obj/bar.o not found for obj/bar.a")
					return &rpb.ActionResult{
						ExitCode:  1,
						StderrRaw: []byte(fmt.Sprintf("obj/bar.o: File not found: %v", err)),
					}, nil
				}
				d, err := reapitest.SetContent(ctx, fakere.CAS, buf.Bytes())
				if err != nil {
					return &rpb.ActionResult{
						ExitCode:  1,
						StderrRaw: []byte(fmt.Sprintf("obj/foo.so: failed to store: %v", err)),
					}, nil
				}
				t.Logf("obj/foo.so => %s", d)
				return &rpb.ActionResult{
					ExitCode: 0,
					OutputFiles: []*rpb.OutputFile{
						{
							Path:   "obj/foo.so",
							Digest: d,
						},
					},
				}, nil

			default:
				return nil, fmt.Errorf("unsupported commandline %q", cmd.Arguments)
			}
		},
	}
	var ds dataSource
	defer func() {
		err := ds.Close(ctx)
		if err != nil {
			t.Error(err)
		}
	}()
	ds.client = reapitest.New(ctx, t, fakere)
	ds.cache = ds.client.CacheStore()

	stats, err := ninja(t, ds)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Total != 7 || stats.Done != stats.Total || stats.Remote != 4 || stats.Skipped != 1 {
		t.Errorf("stats total=%d done=%d remote=%d skipped=%d; want total=7 done=7 remote=4 skipped=1 (%#v)", stats.Total, stats.Done, stats.Remote, stats.Skipped, stats)
	}

	t.Logf("second build. no-op")
	stats, err = ninja(t, ds)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Total != 7 || stats.Done != stats.Total || stats.Remote != 0 || stats.Skipped != stats.Total {
		t.Errorf("stats total=%d done=%d remote=%d skipped=%d; want total=7 done=7 remote=0 skipped=7", stats.Total, stats.Done, stats.Remote, stats.Skipped)
	}

	t.Logf("modify foo.cc")
	err = os.WriteFile(filepath.Join(dir, "foo.cc"), []byte(`
// new content
#include "gen/bar.h"
`), 0644)
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("third build")
	// make sure replace / accumulate still works
	// even if stamp, alink steps are skipped.
	stats, err = ninja(t, ds)
	if err != nil {
		t.Fatal(err)
	}
	// remote: cxx obj/foo.o, solink obj/foo.so
	if stats.Total != 7 || stats.Done != stats.Total || stats.Remote != 2 || stats.Skipped != 5 {
		t.Errorf("stats total=%d done=%d remote=%d skipped=%d; want total=7 done=7 remote=2 skipped=5: (%#v)", stats.Total, stats.Done, stats.Remote, stats.Skipped, stats)
	}

}
