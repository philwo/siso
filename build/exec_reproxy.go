// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	ppb "github.com/bazelbuild/reclient/api/proxy"
	log "github.com/golang/glog"

	"infra/build/siso/execute"
	"infra/build/siso/execute/reproxyexec"
	"infra/build/siso/o11y/clog"
	"infra/build/siso/o11y/trace"
	"infra/build/siso/toolsupport/gccutil"
	"infra/build/siso/toolsupport/msvcutil"
)

func (b *Builder) execReproxy(ctx context.Context, step *Step) error {
	ctx, span := trace.NewSpan(ctx, "exec-reproxy")
	defer span.Close(nil)
	clog.Infof(ctx, "exec reproxy %s", step.cmd.Desc)
	step.setPhase(stepInput)
	// expand inputs to get full action inputs,
	// before preparing inputs on local disk for reproxy.
	depsExpandInputs(ctx, b, step)
	b.fixMissingInputs(ctx, step)
	err := b.prepareLocalInputs(ctx, step)
	if err != nil && !experiments.Enabled("ignore-missing-local-inputs", "step %s missing inputs: %v", step, err) {
		return err
	}
	err = b.prepareLocalIncludeDirs(ctx, step)
	if err != nil {
		return err
	}
	err = b.prepareLocalOutdirs(ctx, step)
	if err != nil {
		return err
	}
	err = allowWriteOutputs(ctx, step.cmd)
	if err != nil {
		return err
	}
	err = b.reproxySema.Do(ctx, func(ctx context.Context) error {
		started := time.Now()
		step.metrics.ActionStartTime = IntervalMetric(started.Sub(b.start))
		clog.Infof(ctx, "step state: remote exec (via reproxy)")
		step.setPhase(stepRemoteRun)
		maybeDisableLocalFallback(ctx, step)
		err := b.reproxyExec.Run(ctx, step.cmd)
		step.setPhase(stepOutput)
		ar, cached := step.cmd.ActionResult()
		switch ar.GetExecutionMetadata().GetWorker() {
		case reproxyexec.WorkerNameFallback:
			step.metrics.IsLocal = true
			step.metrics.Fallback = true
			fallbackResult := step.cmd.RemoteFallbackResult()
			exitCode := -1
			if e := fallbackResult.GetExitCode(); e != 0 {
				exitCode = int(e)
			}
			msgs := cmdOutput(ctx, "FALLBACK", step.cmd, step.def.Binding("command"), step.def.RuleName(), fmt.Errorf("fallback in reproxy exit=%d", exitCode))
			if stdout := fallbackResult.GetStdoutRaw(); len(stdout) > 0 {
				msgs = append(msgs, fmt.Sprintf("stdout:\n%s", string(stdout)))
			}
			if stderr := fallbackResult.GetStderrRaw(); len(stderr) > 0 {
				msgs = append(msgs, fmt.Sprintf("stderr:\n%s", string(stderr)))
			}
			b.logOutput(ctx, msgs, false)
		case reproxyexec.WorkerNameLocal, reproxyexec.WorkerNameRacingLocal:
			// TODO: Siso may want to have `racing`flag in the step metrics.
			step.metrics.IsLocal = true
		default:
			step.metrics.IsRemote = true
		}
		if cached {
			step.metrics.Cached = true
		}
		step.metrics.RunTime = IntervalMetric(time.Since(started))
		step.metrics.done(ctx, step)
		return err
	})
	if err != nil {
		return fmt.Errorf("reproxy error: %w", err)
	}
	// TODO(b/273407069): this won't be useful until we add code to specifically handle the deps log that reproxy returns.
	if err = b.updateDeps(ctx, step); err != nil {
		clog.Warningf(ctx, "failed to update deps: %v", err)
	}
	return b.outputs(ctx, step)
}

// allowWriteOutputs fixes the permissions of the output files if they are not writable.
// TODO: b/299227633 - Remove this workaround after Reproxy fixes the write operation.
func allowWriteOutputs(ctx context.Context, cmd *execute.Cmd) error {
	ctx, span := trace.NewSpan(ctx, "allow-write-outputs")
	defer span.Close(nil)
	for _, out := range cmd.Outputs {
		fname := filepath.Join(cmd.ExecRoot, out)
		fi, err := os.Lstat(fname)
		if errors.Is(err, fs.ErrNotExist) {
			// Do nothing if the file doesn't exist.
			continue
		} else if err != nil {
			// We don't know the filemode. So let it go.
			clog.Warningf(ctx, "failed to stat %s: %v", fname, err)
			continue
		}
		// The file needs to be writable. Otherwise writing the output file fails with permission denied.
		if fi.Mode()&0200 == 0 {
			err = os.Chmod(fname, fi.Mode()|0200)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// prepareLocalIncludeDirs make sure that the include directories in a compile command exist.
// b/289175336 - In Reproxy mode, the same compile command ends up with different input trees
// due to empty input directories. Those directories are created by generator actions,
// but not always used by compile actions.
// It's better to have the include directories regardless of timing.
func (b *Builder) prepareLocalIncludeDirs(ctx context.Context, step *Step) error {
	args := step.cmd.Args

	var err error
	var dirs []string
	switch step.cmd.Deps {
	case "gcc":
		_, dirs, _, _, err = gccutil.ScanDepsParams(ctx, args, nil)
	case "msvc":
		_, _, dirs, _, _, err = msvcutil.ScanDepsParams(ctx, args, nil)
	default:
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to parse compile commmand flags: %w", err)
	}

	seen := make(map[string]bool)
	for _, dir := range dirs {
		if seen[dir] {
			continue
		}
		seen[dir] = true
		if !filepath.IsLocal(dir) {
			// Do not touch source tree or system dirs outside the build dir.
			continue
		}
		d := b.path.MustFromWD(dir)
		if _, err := b.hashFS.Stat(ctx, b.path.ExecRoot, d); errors.Is(err, fs.ErrNotExist) {
			clog.Infof(ctx, "prepare include dir %s", dir)
			err = b.hashFS.Mkdir(ctx, b.path.ExecRoot, d, nil)
			if err != nil {
				clog.Warningf(ctx, "failed to create include dir %s: %v", dir, err)
			}
		}
	}
	return nil
}

func maybeDisableLocalFallback(ctx context.Context, step *Step) {
	// Manually override remote_local_fallback to remote when falback is disabled.
	// TODO: b/297807325 - Siso relies on Reclient metrics and monitoring at this moment.
	// CompileErrorRatioAlert checks remote failure/local success case. So it
	// needs to do local fallback on Reproxy side. However, all local executions
	// need to be handled at Siso layer.
	if experiments.Enabled("no-fallback", "") && strings.ToUpper(step.cmd.REProxyConfig.ExecStrategy) == ppb.ExecutionStrategy_REMOTE_LOCAL_FALLBACK.String() {
		if log.V(1) {
			clog.Infof(ctx, "overriding reproxy REMOTE_LOCAL_FALLBACK to REMOTE")
		}
		step.cmd.REProxyConfig.ExecStrategy = ppb.ExecutionStrategy_REMOTE.String()
	}
}
