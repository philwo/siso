// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"time"

	log "github.com/golang/glog"

	"infra/build/siso/execute"
	"infra/build/siso/o11y/clog"
	"infra/build/siso/o11y/trace"
	"infra/build/siso/ui"
)

// checkUpToDate returns true if outputs are already up-to-date and
// no need to run command.
func (b *Builder) checkUpToDate(ctx context.Context, step *Step) bool {
	ctx, span := trace.NewSpan(ctx, "mtime-check")
	defer span.Close(nil)

	generator := step.def.Binding("generator") != ""
	out0, outmtime, cmdhash := outputMtime(ctx, b, step.cmd)
	lastIn, inmtime, err := inputMtime(ctx, b, step.cmd)

	// TODO(b/288419130): make sure it covers all cases as ninja does.

	outname := b.path.MustToWD(out0)
	lastInName := b.path.MustToWD(lastIn)
	if err != nil {
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
	if !generator && !bytes.Equal(cmdhash, step.cmd.CmdHash) {
		clog.Infof(ctx, "need: cmdhash differ %q -> %q", hex.EncodeToString(cmdhash), hex.EncodeToString(step.cmd.CmdHash))
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
	clog.Infof(ctx, "skip: in:%s < out:%s %s", lastIn, out0, outmtime.Sub(inmtime))
	span.SetAttr("skip", true)
	if b.stats.skipped(ctx)%100 == 0 && ui.IsTerminal() {
		b.progressStepSkipped(ctx, step)
	}
	return true
}

// outputMtime returns the oldest modified output, its timestamp and
// command hash that produced the outputs of the step.
func outputMtime(ctx context.Context, b *Builder, cmd *execute.Cmd) (string, time.Time, []byte) {
	var oerr error
	var outmtime time.Time
	var outcmdhash []byte
	out0 := ""
	for i, out := range cmd.Outputs {
		fi, err := b.hashFS.Stat(ctx, b.path.ExecRoot, out)
		if err != nil {
			if oerr == nil {
				out0 = out
				oerr = err
			}
			continue
		}
		if log.V(1) {
			clog.Infof(ctx, "out-cmdhash %d:%s %s", i, out, hex.EncodeToString(fi.CmdHash()))
		}
		if i == 0 {
			outcmdhash = fi.CmdHash()
		}
		if !bytes.Equal(outcmdhash, fi.CmdHash()) {
			if log.V(1) {
				clog.Infof(ctx, "out-cmdhash differ %s %s->%s", out, hex.EncodeToString(outcmdhash), hex.EncodeToString(fi.CmdHash()))
			}
			outcmdhash = nil
		}
		if outmtime.IsZero() {
			outmtime = fi.ModTime()
			out0 = out
			continue
		}
		if outmtime.After(fi.ModTime()) {
			outmtime = fi.ModTime()
			out0 = out
		}
	}
	if oerr != nil {
		outmtime = time.Time{}
	}
	return out0, outmtime, outcmdhash
}

// inputMtime returns the last modified input and its modified timestamp.
func inputMtime(ctx context.Context, b *Builder, cmd *execute.Cmd) (string, time.Time, error) {
	var inmtime time.Time
	lastIn := ""
	for _, in := range cmd.Inputs {
		fi, err := b.hashFS.Stat(ctx, b.path.ExecRoot, in)
		if err != nil {
			return "", inmtime, fmt.Errorf("missing input %s for %s: %v", cmd, in, err)
		}
		if inmtime.Before(fi.ModTime()) {
			inmtime = fi.ModTime()
			lastIn = in
		}
	}
	return lastIn, inmtime, nil
}
