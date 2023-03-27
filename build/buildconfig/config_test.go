// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package buildconfig

import (
	"context"
	"io/fs"
	"os"
	"testing"
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
			_, err := New(ctx, "@config//main.star", tc.flags, cfgrepos)
			if err != nil {
				t.Errorf(`NewConfig(ctx, "@config//main.star", %v, nil)=_, %v; want nil error`, tc.flags, err)
			}
		})
	}
}
