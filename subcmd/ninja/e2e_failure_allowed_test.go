// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"infra/build/siso/build"
	"infra/build/siso/hashfs"
)

func TestBuild_SwallowFailures(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	setupFiles(t, dir, t.Name(), nil)
	opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{})
	t.Cleanup(cleanup)
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

func TestBuild_SwallowFailuresLimit(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	setupFiles(t, dir, t.Name(), nil)
	opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{})
	t.Cleanup(cleanup)
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

	setupFiles(t, dir, t.Name(), nil)
	opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{})
	t.Cleanup(cleanup)
	opt.FailuresAllowed = 11
	var metricsBuffer bytes.Buffer
	opt.MetricsJSONWriter = &metricsBuffer

	b, err := build.New(ctx, graph, opt)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	err = b.Build(ctx, "build", "all")
	if err == nil {
		t.Fatal(`b.Build(ctx, "build", "all")=nil, want err`)
	}

	stats := b.Stats()
	t.Logf("err %v; %#v", err, stats)
	if got, want := stats.Fail, 2; got != want {
		t.Errorf("stats.Fail=%d; want=%d", got, want)
	}
	if got, want := stats.Done, 10; got != want {
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
