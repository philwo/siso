// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"time"

	log "github.com/golang/glog"

	"infra/build/siso/o11y/clog"
	"infra/build/siso/o11y/trace"
	ppb "infra/third_party/reclient/api/proxy"
)

func (b *Builder) runReproxy(ctx context.Context, step *Step) error {
	ctx, span := trace.NewSpan(ctx, "run-reproxy")
	defer span.Close(nil)
	clog.Infof(ctx, "run reproxy %s", step.cmd.Desc)
	step.setPhase(stepInput)
	// expand inputs to get full action inputs,
	// before preparing inputs on local disk for reproxy.
	depsExpandInputs(ctx, b, step)
	err := b.prepareLocalInputs(ctx, step)
	if err != nil && !experiments.Enabled("ignore-missing-local-inputs", "step %s missing inputs: %v", step, err) {
		return err
	}
	err = b.prepareLocalOutdirs(ctx, step)
	if err != nil {
		return err
	}
	err = b.reproxySema.Do(ctx, func(ctx context.Context) error {
		started := time.Now()
		clog.Infof(ctx, "step state: remote exec (via reproxy)")
		step.setPhase(stepRemoteRun)
		maybeDisableLocalFallback(ctx, step)
		err := b.reproxyExec.Run(ctx, step.cmd)
		step.setPhase(stepOutput)
		if err == nil {
			step.metrics.IsRemote = true
		}
		_, cached := step.cmd.ActionResult()
		if cached {
			b.stats.cacheHit(ctx)
		} else {
			b.stats.remoteDone(ctx, err) // use other stats for reproxy?
		}
		step.metrics.RunTime = IntervalMetric(time.Since(started))
		step.metrics.done(ctx, step)
		return err
	})
	if err != nil {
		return err
	}
	// TODO(b/273407069): this won't be useful until we add code to specifically handle the deps log that reproxy returns.
	if err = b.updateDeps(ctx, step); err != nil {
		clog.Warningf(ctx, "failed to update deps: %v", err)
	}
	return err
}

func maybeDisableLocalFallback(ctx context.Context, step *Step) {
	// Manually override remote_local_fallback to remote when falback is disabled.
	// TODO: b/297807325 - Siso relies on Reclient metrics and monitoring at this moment.
	// CompileErrorRatioAlert checks remote failure/local success case. So it
	// needs to do local fallback on Reproxy side. However, all local executions
	// need to be handled at Siso layer.
	if experiments.Enabled("no-fallback", "") && step.cmd.REProxyConfig.ExecStrategy == ppb.ExecutionStrategy_REMOTE_LOCAL_FALLBACK.String() {
		if log.V(1) {
			clog.Infof(ctx, "overriding reproxy REMOTE_LOCAL_FALLBACK to REMOTE")
		}
		step.cmd.REProxyConfig.ExecStrategy = ppb.ExecutionStrategy_REMOTE.String()
	}
}
