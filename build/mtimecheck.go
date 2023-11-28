// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	log "github.com/golang/glog"

	"infra/build/siso/o11y/clog"
	"infra/build/siso/o11y/trace"
)

// checkUpToDate returns true if outputs are already up-to-date and
// no need to run command.
func (b *Builder) checkUpToDate(ctx context.Context, stepDef StepDef, outputs []Target) bool {
	ctx, span := trace.NewSpan(ctx, "mtime-check")
	defer span.Close(nil)

	generator := stepDef.Binding("generator") != ""
	cmdline := stepDef.Binding("command")
	rspfileContent := stepDef.Binding("rspfile_content")
	stepCmdHash := calculateCmdHash(cmdline, rspfileContent)

	out0, outmtime, cmdhash := outputMtime(ctx, b, outputs, stepDef.Binding("restat") != "")
	lastIn, inmtime, err := inputMtime(ctx, b, stepDef)

	// TODO(b/288419130): make sure it covers all cases as ninja does.

	outname := b.path.MaybeToWD(out0)
	lastInName := b.path.MaybeToWD(lastIn)
	if errors.Is(err, errDirty) {
		clog.Infof(ctx, "need %v", err)
		span.SetAttr("run-reason", "dirty")
		fmt.Fprintf(b.explainWriter, "deps for %s is dirty: %v\n", outname, err)
	} else if err != nil {
		// missing inputs in deps file? maybe need to rebuild.
		clog.Infof(ctx, "need: %v", err)
		span.SetAttr("run-reason", "missing-inputs")
		fmt.Fprintf(b.explainWriter, "deps for %s is missing: %v\n", outname, err)
		return false
	}
	if outmtime.IsZero() {
		clog.Infof(ctx, "need: output doesn't exist")
		span.SetAttr("run-reason", "no-output")
		fmt.Fprintf(b.explainWriter, "output %s doesn't exist\n", outname)
		return false
	}
	if inmtime.After(outmtime) {
		clog.Infof(ctx, "need: in:%s > out:%s %s: in:%s out:%s", lastIn, out0, inmtime.Sub(outmtime), inmtime, outmtime)
		span.SetAttr("run-reason", "dirty")
		fmt.Fprintf(b.explainWriter, "output %s older than most recent input %s: out:%s in:+%s\n", outname, lastInName, outmtime.Format(time.RFC3339), inmtime.Sub(outmtime))
		return false
	}
	if !generator && !bytes.Equal(cmdhash, stepCmdHash) {
		clog.Infof(ctx, "need: cmdhash differ %q -> %q", hex.EncodeToString(cmdhash), hex.EncodeToString(stepCmdHash))
		span.SetAttr("run-reason", "cmdhash-update")
		if len(cmdhash) == 0 {
			fmt.Fprintf(b.explainWriter, "command line not found in log for %s\n", outname)
		} else {
			fmt.Fprintf(b.explainWriter, "command line changed for %s\n", outname)
		}
		return false
	}
	if b.clobber {
		clog.Infof(ctx, "need: clobber")
		span.SetAttr("run-reason", "clobber")
		// explain once at the beginning of the build.
		return false
	}
	if b.outputLocal != nil {
		numOuts := len(outputs)
		depFile := stepDef.Depfile()
		if depFile != "" {
			numOuts++
		}
		localOutputs := make([]string, 0, numOuts)
		seen := make(map[string]bool)
		for _, out := range outputs {
			outPath, err := b.graph.TargetPath(out)
			if err != nil {
				clog.Warningf(ctx, "bad target %v", err)
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
				clog.Infof(ctx, "need: no local outputs %q: %v", localOutputs, err)
				span.SetAttr("run-reason", "missing-local-outputs")
				fmt.Fprintf(b.explainWriter, "output %s flush error %s: %v", outname, localOutputs, err)
				return false
			}
			if log.V(1) {
				clog.Infof(ctx, "flush all outputs %s", localOutputs)
			}
		}
	}

	if log.V(1) {
		clog.Infof(ctx, "skip: in:%s < out:%s %s", lastIn, out0, outmtime.Sub(inmtime))
	}
	span.SetAttr("skip", true)
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
		outPath, err := b.graph.TargetPath(out)
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
		if log.V(1) {
			clog.Infof(ctx, "out-cmdhash %d:%s %s", i, outPath, hex.EncodeToString(fi.CmdHash()))
		}
		if i == 0 {
			outcmdhash = fi.CmdHash()
		}
		if !bytes.Equal(outcmdhash, fi.CmdHash()) {
			if log.V(1) {
				clog.Infof(ctx, "out-cmdhash differ %s %s->%s", outPath, hex.EncodeToString(outcmdhash), hex.EncodeToString(fi.CmdHash()))
			}
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
	ins, err := stepDef.TriggerInputs(ctx)
	if err != nil {
		return "", inmtime, fmt.Errorf("failed to load deps: %w", err)
	}
	for _, in := range ins {
		fi, err := b.hashFS.Stat(ctx, b.path.ExecRoot, in)
		if err != nil {
			return "", inmtime, fmt.Errorf("missing input %s: %w", in, err)
		}
		if inmtime.Before(fi.ModTime()) {
			inmtime = fi.ModTime()
			lastIn = in
		}
		if fi.IsUpdated() {
			return "", inmtime, fmt.Errorf("input %s: %w", in, errDirty)
		}
	}
	return lastIn, inmtime, nil
}
