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
			log.Infof("phony output %s dirty=%v mtime=%v", outpath, dirtyErr, mtime)
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

	out0, outmtime, cmdhash := outputMtime(ctx, b, outputs, stepDef.Binding("restat") != "")
	lastIn, inmtime, err := inputMtime(ctx, b, stepDef)

	// TODO(b/288419130): make sure it covers all cases as ninja does.

	outname := b.path.MaybeToWD(out0)
	lastInName := b.path.MaybeToWD(lastIn)
	if err != nil {
		log.Infof("need %v", err)
		reason := "missing-inputs"
		switch {
		case errors.Is(err, ErrMissingDeps):
			reason = "missing-deps"
		case errors.Is(err, ErrStaleDeps):
			reason = "stale-deps"
		case errors.Is(err, errDirty):
			reason = "dirty"
		}
		fmt.Fprintf(b.explainWriter, "deps for %s %s: %v\n", outname, reason, err)
		return false
	}
	if outmtime.IsZero() {
		log.Infof("need: output doesn't exist")
		fmt.Fprintf(b.explainWriter, "output %s doesn't exist\n", outname)
		return false
	}
	if inmtime.After(outmtime) {
		log.Infof("need: in:%s > out:%s %s: in:%s out:%s", lastIn, out0, inmtime.Sub(outmtime), inmtime, outmtime)
		fmt.Fprintf(b.explainWriter, "output %s older than most recent input %s: out:%s in:+%s\n", outname, lastInName, outmtime.Format(time.RFC3339), inmtime.Sub(outmtime))
		return false
	}
	if !generator && !bytes.Equal(cmdhash, stepCmdHash) {
		log.Infof("need: cmdhash differ %q -> %q", base64.StdEncoding.EncodeToString(cmdhash), base64.StdEncoding.EncodeToString(stepCmdHash))
		if len(cmdhash) == 0 {
			fmt.Fprintf(b.explainWriter, "command line not found in log for %s\n", outname)
		} else {
			fmt.Fprintf(b.explainWriter, "command line changed for %s\n", outname)
		}
		return false
	}
	if b.clobber {
		log.Infof("need: clobber")
		// explain once at the beginning of the build.
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
			case "gcc", "msvc":
			default:
				if b.outputLocal(ctx, depFile) {
					localOutputs = append(localOutputs, depFile)
				}
			}
		}
		if len(localOutputs) > 0 {
			err := b.hashFS.Flush(ctx, b.path.ExecRoot, localOutputs)
			if err != nil {
				log.Infof("need: no local outputs %q: %v", localOutputs, err)
				fmt.Fprintf(b.explainWriter, "output %s flush error %s: %v", outname, localOutputs, err)
				return false
			}
			log.Debugf("flush all outputs %s", localOutputs)
		}
	}

	log.Debugf("skip: in:%s < out:%s %s", lastIn, out0, outmtime.Sub(inmtime))
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
		log.Debugf("out-cmdhash %d:%s %s", i, outPath, base64.StdEncoding.EncodeToString(fi.CmdHash()))
		if i == 0 {
			outcmdhash = fi.CmdHash()
		}
		if !bytes.Equal(outcmdhash, fi.CmdHash()) {
			log.Debugf("out-cmdhash differ %s %s->%s", outPath, base64.StdEncoding.EncodeToString(outcmdhash), base64.StdEncoding.EncodeToString(fi.CmdHash()))
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
