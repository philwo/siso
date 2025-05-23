// Copyright 2025 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
)

// stepManifest is a manifest of a step.
// It holds, step's command line, inputs and output of the step,
// and used to check step's re-validation with their hashes.
// TODO: unify cmd hash and edge hash into one hash.
type stepManifest struct {
	// command line of the step.
	cmdline string
	// rspfile content of the step.
	rspfileContent string
	// hash of cmdline and rspfileContent.
	cmdHash []byte

	// inputs of the step.
	inputs []string
	// outputs of the step.
	outputs []string
	// hash of inputs/outputs.
	edgeHash []byte
}

func newStepManifest(ctx context.Context, stepDef StepDef) *stepManifest {
	cmdline := stepDef.Binding("command")
	rspfileContent := stepDef.Binding("rspfile_content")
	inputs := stepDef.TriggerInputs(ctx)
	outputs := stepDef.Outputs(ctx)
	return &stepManifest{
		cmdline:        cmdline,
		rspfileContent: rspfileContent,
		cmdHash:        calculateCmdHash(cmdline, rspfileContent),
		inputs:         inputs,
		outputs:        outputs,
		edgeHash:       calculateEdgeHash(inputs, outputs),
	}
}

func calculateOldCmdHash(cmdline, rspfileContent string) []byte {
	h := sha256.New()
	fmt.Fprint(h, cmdline)
	if rspfileContent != "" {
		fmt.Fprint(h, rspfileContent)
	}
	return h.Sum(nil)
}

const unitSeparator = "\x1f"

func calculateCmdHash(cmdline, rspfileContent string) []byte {
	h := sha256.New()
	io.WriteString(h, cmdline)
	if rspfileContent != "" {
		io.WriteString(h, unitSeparator)
		io.WriteString(h, rspfileContent)
	}
	return h.Sum(nil)
}

func calculateEdgeHash(inputs, outputs []string) []byte {
	h := sha256.New()
	for _, fname := range inputs {
		io.WriteString(h, fname)
		io.WriteString(h, unitSeparator)
	}
	io.WriteString(h, unitSeparator)
	for _, fname := range outputs {
		io.WriteString(h, fname)
		io.WriteString(h, unitSeparator)
	}
	return h.Sum(nil)
}
