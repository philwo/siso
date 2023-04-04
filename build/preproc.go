// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"

	"infra/build/siso/o11y/clog"
	"infra/build/siso/o11y/trace"
)

func fastDepsCmd(ctx context.Context, b *Builder, step *Step) (*Step, bool) {
	ctx, span := trace.NewSpan(ctx, "fast-deps")
	defer span.Close(nil)
	fastStep, err := depsFastStep(ctx, b, step)
	if err != nil {
		clog.Infof(ctx, "no fast-deps %s: %v", step.Cmd.Deps, err)
		return nil, false
	}
	return fastStep, true
}

func preprocCmd(ctx context.Context, b *Builder, step *Step) {
	ctx, span := trace.NewSpan(ctx, "preproc")
	defer span.Close(nil)
	err := depsCmd(ctx, b, step)
	if err != nil {
		clog.Warningf(ctx, "failed to get %s deps: %v", step.Cmd.Deps, err)
	}
}

func uniqueFiles(inputsList ...[]string) []string {
	seen := make(map[string]bool)
	var inputs []string
	for _, ins := range inputsList {
		for _, in := range ins {
			if in == "" {
				continue
			}
			if seen[in] {
				continue
			}
			seen[in] = true
			inputs = append(inputs, in)
		}
	}
	r := make([]string, len(inputs))
	copy(r, inputs)
	return r
}
