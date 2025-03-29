// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"errors"
	"time"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/charmbracelet/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"go.chromium.org/infra/build/siso/reapi"
	"go.chromium.org/infra/build/siso/reapi/retry"
)

func (b *Builder) execRemote(ctx context.Context, step *Step) error {
	var timeout time.Duration
	step.cmd.RecordPreOutputs(ctx)
	phase := stepRemoteRun
	var reExecDur time.Duration
	err := retry.Do(ctx, func() error {
		step.setPhase(phase.wait())
		err := b.remoteSema.Do(ctx, func() error {
			step.setPhase(phase)
			if phase == stepRetryRun {
				b.progressStepRetry(step)
			}
			ctx = reapi.NewContext(ctx, &rpb.RequestMetadata{
				ActionId:                step.cmd.ID,
				ToolInvocationId:        b.id,
				CorrelatedInvocationsId: b.jobID,
				ActionMnemonic:          step.def.ActionName(),
				TargetId:                step.cmd.Outputs[0],
			})
			phase = stepRetryRun
			err := b.remoteExec.Run(ctx, step.cmd)
			step.setPhase(stepOutput)
			result, _ := step.cmd.ActionResult()
			if err == nil && !validateRemoteActionResult(result) {
				log.Errorf("no outputs in action result. retry without cache lookup. b/350360391")
				res := cmdOutput(ctx, cmdOutputResultRETRY, step.cmd, step.def.Binding("command"), step.def.RuleName(), err)
				b.logOutput(res, false)
				step.cmd.SkipCacheLookup = true
				step.setPhase(phase)
				err = b.remoteExec.Run(ctx, step.cmd)
				step.setPhase(stepOutput)
				result, _ = step.cmd.ActionResult()
				if err == nil && !validateRemoteActionResult(result) {
					log.Errorf("no outputs in action result again. b/350360391")
				}
			}
			return err
		})
		if code := status.Code(err); code == codes.DeadlineExceeded || errors.Is(err, context.DeadlineExceeded) && reExecDur < timeout {
			log.Warnf("remote execution timed out: timeout=%s err=%v", timeout, err)
			err = status.Errorf(codes.Unavailable, "reapi timedout %v", err)
		}
		return err
	})
	if err != nil {
		return err
	}
	// need to update deps for remote exec for deps=gcc with depsfile
	if err = b.updateDeps(ctx, step); err != nil {
		return err
	}
	return b.outputs(ctx, step)
}

func (b *Builder) execRemoteCache(ctx context.Context, step *Step) error {
	err := b.cache.GetActionResult(ctx, step.cmd)
	if err != nil {
		return err
	}
	result, _ := step.cmd.ActionResult()
	// result may be nil if GetActionResult detects
	// "skip: no need to update", i.e. all outputs
	// generated by the same action digest.
	if result != nil && !validateRemoteActionResult(result) {
		log.Errorf("no outputs in action result. ignore cache lookup. b/350360391")
		step.cmd.SkipCacheLookup = true
		return errors.New("no output in action result")
	}
	b.progressStepCacheHit(step)
	// need to update deps for cache hit for deps=gcc.
	// even if cache hit, deps should be updated with gcc depsfile.
	if err = b.updateDeps(ctx, step); err != nil {
		return err
	}
	return b.outputs(ctx, step)
}
