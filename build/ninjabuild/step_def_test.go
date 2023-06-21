// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjabuild

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"

	"infra/build/siso/build"
)

func TestStepExpandLabels(t *testing.T) {
	ctx := context.Background()
	g := &globals{
		path: build.NewPath("/b/w", "out/Default"),
		stepConfig: &StepConfig{
			InputDeps: map[string][]string{
				"component:component": {
					"component/a:a",
					"component/b",
				},
				"component/a:a": {
					"component/a/1",
					"component/a/2",
				},
			},
		},
	}
	s := &StepDef{
		globals: g,
	}

	got := s.expandLabels(ctx, []string{
		"foo/bar",
		"component:component",
	})
	want := []string{
		"foo/bar",
		"component/b",
		"component/a/1",
		"component/a/2",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("s.expandLabels(...): diff -want +got:\n%s", diff)
	}
}
