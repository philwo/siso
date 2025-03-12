// Copyright 2025 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/infra/build/siso/build"
	"go.chromium.org/infra/build/siso/hashfs"
	"go.chromium.org/infra/build/siso/reapi/reapitest"
)

func TestBuild_RemoveStaleOutput(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	ninja := func(t *testing.T, outputLocal bool, out io.Writer, fakere *reapitest.Fake) (build.Stats, error) {
		t.Helper()
		var ds dataSource
		defer func() {
			err := ds.Close(ctx)
			if err != nil {
				t.Error(err)
			}
		}()
		ds.client = reapitest.New(ctx, t, fakere)
		ds.cache = ds.client.CacheStore()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:   ".siso_fs_state",
			OutputLocal: func(context.Context, string) bool { return outputLocal },
			DataSource:  ds,
		})
		defer cleanup()
		opt.REAPIClient = ds.client
		opt.OutputLocal = func(context.Context, string) bool { return outputLocal }
		opt.OutputLogWriter = out
		return runNinja(ctx, "build.ninja", graph, opt, nil, runNinjaOpts{})
	}
	setupFiles(t, dir, t.Name(), nil)

	fakere := &reapitest.Fake{
		ExecuteFunc: func(fakere *reapitest.Fake, action *rpb.Action) (*rpb.ActionResult, error) {
			tree := reapitest.InputTree{CAS: fakere.CAS, Root: action.InputRootDigest}
			fn, err := tree.LookupFileNode(ctx, "input")
			if err != nil {
				t.Logf("missing input: %v", err)
				return &rpb.ActionResult{
					ExitCode:  1,
					StderrRaw: []byte(fmt.Sprintf("../../input: File not found: %v", err)),
				}, nil
			}
			return &rpb.ActionResult{
				ExitCode: 0,
				OutputFiles: []*rpb.OutputFile{
					{
						Path:   "out0",
						Digest: fn.Digest,
					},
					{
						Path:   "out1",
						Digest: fn.Digest,
					},
				},
			}, nil
		},
	}

	t.Logf("-- first build. output_local=true")
	var out bytes.Buffer
	stats, err := ninja(t, true, &out, fakere)
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}
	if stats.Done != stats.Total || stats.Total != 2 || stats.Remote != 1 || stats.Local != 1 {
		t.Errorf("done=%d total=%d remote=%d local=%d; want done=total=2 remote=local=1: %#v", stats.Done, stats.Total, stats.Remote, stats.Local, stats)
	}

	t.Logf("-- confirm no op")
	stats, err = ninja(t, true, &out, fakere)
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}
	if stats.Done != stats.Total || stats.Total != 2 || stats.Skipped != 2 {
		t.Errorf("done=%d total=%d skipped=%d; want done=total=skipped=2; %#v", stats.Done, stats.Total, stats.Skipped, stats)
	}

	t.Logf("-- modify input")
	modifyFile(t, dir, "input", func(b []byte) []byte {
		return append(b, []byte(" modified")...)
	})

	t.Logf("-- second build. output_local=false. expect step B fail with missing inputs")
	stats, err = ninja(t, false, &out, fakere)
	if err == nil {
		t.Fatalf("ninja passed; want failure")
	}
	if stats.Fail != 1 {
		t.Errorf("fail=%d; want fail=1; %#v", stats.Fail, stats)
	}
	t.Logf("out:\n%s", out.String())
	if !strings.Contains(out.String(), "No such file or directory: 'out1'") {
		t.Error("expect - No such file or directory: 'out1'")
	}
	_, err = os.Stat(filepath.Join(dir, "out/siso/out1"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stat out/siso/out1 %v; want %v", err, fs.ErrNotExist)
	}
}
