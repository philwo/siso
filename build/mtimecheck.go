// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/charmbracelet/log"
)

type phonyState struct {
	dirtyErr error
	mtime    time.Time
}

// needToRun checks whether step needs to run or not.
func (b *Builder) needToRun(ctx context.Context, stepDef StepDef, outputs []Target) bool {
	if stepDef.IsPhony() {
		var dirtyErr error
		_, mtime, err := inputMtime(ctx, b, stepDef)

		if errors.Is(err, errDirty) {
			// if phony's inputs are dirty, mark this phony's output as dirty.
			dirtyErr = err
		}
		for _, out := range outputs {
			outpath, err := b.graph.TargetPath(ctx, out)
			if err != nil {
				log.Warnf("failed to get targetpath for %v: %v", out, err)
				continue
			}
			b.phony.Store(outpath, phonyState{
				dirtyErr: dirtyErr,
				mtime:    mtime,
			})
		}
		// nothing to run for phony target.
		return false
	}
	return !b.checkUpToDate(ctx, stepDef, outputs)
}

// checkUpToDate returns true if outputs are already up-to-date and
// no need to run command.
func (b *Builder) checkUpToDate(ctx context.Context, stepDef StepDef, outputs []Target) bool {
	generator := stepDef.Binding("generator") != ""
	cmdline := stepDef.Binding("command")
	rspfileContent := stepDef.Binding("rspfile_content")
	stepCmdHash := calculateCmdHash(cmdline, rspfileContent)

	_, outmtime, cmdhash := outputMtime(ctx, b, outputs, stepDef.Binding("restat") != "")
	_, inmtime, err := inputMtime(ctx, b, stepDef)

	// TODO(b/288419130): make sure it covers all cases as ninja does.
	if err != nil {
		return false
	}
	if outmtime.IsZero() {
		return false
	}
	if inmtime.After(outmtime) {
		return false
	}
	if !generator && !bytes.Equal(cmdhash, stepCmdHash) {
		return false
	}
	if b.clobber {
		return false
	}
	if b.outputLocal != nil {
		numOuts := len(outputs)
		depFile := stepDef.Depfile(ctx)
		if depFile != "" {
			numOuts++
		}
		localOutputs := make([]string, 0, numOuts)
		seen := make(map[string]bool)
		for _, out := range outputs {
			outPath, err := b.graph.TargetPath(ctx, out)
			if err != nil {
				log.Warnf("bad target %v", err)
				continue
			}
			if seen[outPath] {
				continue
			}
			seen[outPath] = true
			if !b.outputLocal(ctx, outPath) {
				continue
			}
			localOutputs = append(localOutputs, outPath)
		}
		if depFile != "" {
			switch stepDef.Binding("deps") {
			case "gcc":
			default:
				if b.outputLocal(ctx, depFile) {
					localOutputs = append(localOutputs, depFile)
				}
			}
		}
		if len(localOutputs) > 0 {
			err := b.hashFS.Flush(ctx, b.path.ExecRoot, localOutputs)
			if err != nil {
				return false
			}
		}
	}

	return true
}

// outputMtime returns the oldest modified output, its timestamp and
// command hash that produced the outputs of the step.
func outputMtime(ctx context.Context, b *Builder, outputs []Target, restat bool) (string, time.Time, []byte) {
	var oerr error
	var outmtime time.Time
	var outcmdhash []byte
	out0 := ""
	for i, out := range outputs {
		outPath, err := b.graph.TargetPath(ctx, out)
		if err != nil {
			if oerr == nil {
				oerr = err
			}
			continue
		}
		fi, err := b.hashFS.Stat(ctx, b.path.ExecRoot, outPath)
		if err != nil {
			if oerr == nil {
				out0 = outPath
				oerr = err
			}
			continue
		}
		if i == 0 {
			outcmdhash = fi.CmdHash()
		}
		if !bytes.Equal(outcmdhash, fi.CmdHash()) {
			outcmdhash = nil
		}
		var t time.Time
		if restat {
			t = fi.UpdatedTime()
		} else {
			t = fi.ModTime()
		}
		if outmtime.IsZero() {
			outmtime = t
			out0 = outPath
			continue
		}
		if outmtime.After(t) {
			outmtime = t
			out0 = outPath
		}
	}
	if oerr != nil {
		outmtime = time.Time{}
	}
	return out0, outmtime, outcmdhash
}

var errDirty = errors.New("dirty")

// inputMtime returns the last modified input and its modified timestamp.
// it will return errDirty if input file has been updated in the build session.
func inputMtime(ctx context.Context, b *Builder, stepDef StepDef) (string, time.Time, error) {
	var inmtime time.Time
	lastIn := ""
	ins := stepDef.TriggerInputs(ctx)
	depsIter, err := stepDef.DepInputs(ctx)
	if err != nil {
		return "", inmtime, fmt.Errorf("failed to load deps: %w", err)
	}
	seen := make(map[string]bool)

	var retErr error
	appendSeq(ins, depsIter)(func(in string) bool {
		if seen[in] {
			return true
		}
		seen[in] = true
		var mtime time.Time
		var changed bool
		v, ok := b.phony.Load(in)
		if ok {
			// phony target
			ps, ok := v.(phonyState)
			if !ok {
				retErr = fmt.Errorf("unexpected value in dirtyPhony for %s: %T", in, v)
				return false
			}
			if ps.dirtyErr != nil {
				retErr = fmt.Errorf("input %s (phony): %w", in, ps.dirtyErr)
				return false
			}
			mtime = ps.mtime
		} else {
			fi, err := b.hashFS.Stat(ctx, b.path.ExecRoot, in)
			if err != nil {
				retErr = fmt.Errorf("missing input %s: %w", in, err)
				return false
			}
			mtime = fi.ModTime()
			changed = fi.IsChanged()
		}
		if inmtime.Before(mtime) {
			inmtime = mtime
			lastIn = in
		}
		if changed {
			retErr = fmt.Errorf("input %s: %w", in, errDirty)
			return false
		}
		return true
	})
	if retErr != nil {
		return "", inmtime, retErr
	}
	return lastIn, inmtime, nil
}

func appendSeq(ins []string, iter func(func(string) bool)) func(yield func(string) bool) {
	return func(yield func(string) bool) {
		for _, in := range ins {
			if !yield(in) {
				return
			}
		}
		iter(yield)
	}
}
