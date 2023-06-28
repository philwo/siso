// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"testing"

	"infra/build/siso/execute"
	"infra/build/siso/hashfs"
)

func TestDone_DryRun(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}

	b := &Builder{
		hashFS: hashFS,
		stats:  &stats{},
		plan: &plan{
			q: make(chan *Step),
		},
		dryRun: true,
	}

	step := &Step{
		def: fakeStepDef{
			outputs: []string{"out/siso/gen/out.o"},
		},
		cmd: &execute.Cmd{
			ExecRoot: dir,
			Outputs:  []string{"out/siso/gen/out.o"},
		},
	}

	err = b.done(ctx, step)
	if err != nil {
		t.Errorf("b.done(ctx, step)=%v; want nil err", err)
	}
}
