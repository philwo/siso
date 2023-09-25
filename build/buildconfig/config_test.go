// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package buildconfig

import (
	"context"
	"io/fs"
	"os"
	"runtime"
	"testing"

	"infra/build/siso/build"
	"infra/build/siso/execute"
	"infra/build/siso/hashfs"
)

func TestConfig(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name  string
		flags map[string]string
	}{
		{
			name: "basic",
			flags: map[string]string{
				"dir":    "out/Default",
				"target": "",
			},
		},
		{
			name: "config=local_link",
			flags: map[string]string{
				"dir":    "out/Default",
				"config": "local_link",
				"target": "",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfgrepos := map[string]fs.FS{
				"config": os.DirFS("./testdata"),
			}
			cfg, err := New(ctx, "@config//main.star", tc.flags, cfgrepos)
			if err != nil {
				t.Errorf(`New(ctx, "@config//main.star", %v, nil)=_, %v; want nil error`, tc.flags, err)
			}

			fs, err := hashfs.New(ctx, hashfs.Option{})
			if err != nil {
				t.Fatalf(`Failed to create hashfs. err=%v`, err)
			}
			defer func() {
				err = fs.Close(ctx)
				if err != nil {
					t.Fatalf("hfs.Close=%v", err)
				}
			}()
			bpath := build.NewPath("/root", "out/Default")
			_, err = cfg.Init(ctx, fs, bpath)
			if err != nil {
				t.Errorf(`cfg.Init()=%v; want nil error`, err)
			}
		})
	}
}

func TestConfigHandler(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux only test.")
	}

	ctx := context.Background()

	flags := map[string]string{
		"dir":    "out/Default",
		"config": "foo,bar",
		"target": "",
	}
	cfgrepos := map[string]fs.FS{
		"config": os.DirFS("./testdata"),
	}
	cfg, err := New(ctx, "@config//main.star", flags, cfgrepos)
	if err != nil {
		t.Errorf(`New(ctx, "@config//main.star", %v, nil)=_, %v; want nil error`, flags, err)
	}

	fs, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatalf(`Failed to create hashfs. err=%v`, err)
	}
	defer func() {
		err = fs.Close(ctx)
		if err != nil {
			t.Fatalf("hfs.Close=%v", err)
		}
	}()
	bpath := build.NewPath("/root", "out/Default")
	_, err = cfg.Init(ctx, fs, bpath)
	if err != nil {
		t.Errorf(`cfg.Init()=%v; want nil error`, err)
	}

	cmd := &execute.Cmd{}
	err = cfg.Handle(ctx, "handler_foo", bpath, cmd, nil)
	if err != nil {
		t.Errorf(`cfg.Handle()=%v; want nil error`, err)
	}
}
