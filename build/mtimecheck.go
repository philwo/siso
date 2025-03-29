// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"bytes"
	"context"
	"encoding/base64"
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
func (b *Builder) needToRun(ctx context.Context, stepDef StepDef, stepManifest *stepManifest) bool {
	if stepDef.IsPhony() {
		var dirtyErr error
		_, mtime, err := inputMtime(ctx, b, stepDef)

		if errors.Is(err, errDirty) {
			// if phony's inputs are dirty, mark this phony's output as dirty.
			dirtyErr = err
		}
		for _, outpath := range stepManifest.outputs {
			b.phony.Store(outpath, phonyState{
				dirtyErr: dirtyErr,
				mtime:    mtime,
			})
		}
		// nothing to run for phony target.
		return false
	}
	return !b.checkUpToDate(ctx, stepDef, stepManifest)
}

// checkUpToDate returns true if outputs are already up-to-date and
// no need to run command.
func (b *Builder) checkUpToDate(ctx context.Context, stepDef StepDef, stepManifest *stepManifest) bool {
	generator := stepDef.Binding("generator") != ""

	_, outmtime, cmdhash, edgehash := outputMtime(ctx, b, stepManifest.outputs, stepDef.Binding("restat") != "")
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
	if !generator && !bytes.Equal(cmdhash, stepManifest.cmdHash) {
		// TODO: remove old cmdhash support
		oldCmdHash := calculateOldCmdHash(stepManifest.cmdline, stepManifest.rspfileContent)
		if !bytes.Equal(cmdhash, oldCmdHash) {
			return false
		}
		// match with old cmd hash.
	}
	if len(edgehash) == 0 {
		// old version of siso didn't record edgehash...
		// TODO: remove this condition?
		log.Warnf("missing edgehash in output")
	} else if !bytes.Equal(edgehash, stepManifest.edgeHash) {
		log.Infof("need: edgehash differ %q -> %q", base64.StdEncoding.EncodeToString(edgehash), base64.StdEncoding.EncodeToString(stepManifest.edgeHash))
		return false
	}
	if b.clobber {
		return false
	}
	if b.outputLocal != nil {
		numOuts := len(stepManifest.outputs)
		depFile := stepDef.Depfile(ctx)
		if depFile != "" {
			numOuts++
		}
		localOutputs := make([]string, 0, numOuts)
		seen := make(map[string]bool)
		for _, outPath := range stepManifest.outputs {
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
// command hash / edge hash that produced the outputs of the step.
func outputMtime(ctx context.Context, b *Builder, outputs []string, restat bool) (string, time.Time, []byte, []byte) {
	var oerr error
	var outmtime time.Time
	var outcmdhash []byte
	var edgehash []byte
	out0 := ""
	for i, outPath := range outputs {
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
			edgehash = fi.EdgeHash()
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
	return out0, outmtime, outcmdhash, edgehash
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
	var retErr error
	appendSeq(ins, depsIter)(func(in string) bool {
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
	// ins should be unique inputs.
	// iter (deps log) should also be unique inputs,
	// but input in iter may be duplicated with input in ins.
	seen := make(map[string]bool, len(ins))
	return func(yield func(string) bool) {
		for _, in := range ins {
			seen[in] = true
			if !yield(in) {
				return
			}
		}
		iter(func(in string) bool {
			if seen[in] {
				return true
			}
			return yield(in)
		})
	}
}
