// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"errors"
	"fmt"

	"github.com/charmbracelet/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"go.chromium.org/infra/build/siso/reapi"
)

var errDepsLog = errors.New("failed to exec with deps log")
var errNeedPreproc = errors.New("need to preproc")
var errRemoteExecDisabled = errors.New("remote exec disabled")

// runRemote runs step with using remote apis.
func (b *Builder) runRemote(ctx context.Context, step *Step) error {
	preprocErr := errNeedPreproc
	needCheckCache := true
	cacheCheck := b.cache != nil && b.reCacheEnableRead
	startLocal := b.startLocalCounter.Add(-1) >= 0
	if startLocal {
		// no cacheCheck as startlocal for incremental build
		// will build modified code, and not expect cache hit (?)
		log.Infof("start local %s", step.cmd.Desc)
		err := b.execLocal(ctx, step)
		step.metrics.StartLocal = true
		return err
	}
	if preprocErr == errNeedPreproc {
		preprocErr = preprocCmd(ctx, b, step)
	}
	err := preprocErr
	if err == nil {
		err = b.runRemoteStep(ctx, step, needCheckCache && cacheCheck)
	}
	dedupInputs(step.cmd)
	err = b.runRemoteStep(ctx, step, cacheCheck)
	if err != nil {
		if errors.Is(err, errRemoteExecDisabled) {
			return b.execLocal(ctx, step)
		}
		if errors.Is(err, context.Canceled) {
			return err
		}
		if errors.Is(err, reapi.ErrBadPlatformContainerImage) {
			return err
		}
		if status.Code(err) == codes.PermissionDenied {
			return err
		}
		if errors.Is(err, errNotRelocatable) {
			log.Errorf("not relocatable: %v", err)
			return err
		}
		return fmt.Errorf("remote-exec %s failed: %w", step.cmd.ActionDigest(), err)
	}
	return err
}

func (b *Builder) runRemoteStep(ctx context.Context, step *Step, cacheCheck bool) error {
	if len(step.cmd.Platform) == 0 {
		return fmt.Errorf("no remote available (missing platform property)")
	}
	if cacheCheck {
		err := b.execRemoteCache(ctx, step)
		if err == nil {
			return nil
		}
	}
	if !b.reExecEnable {
		return errRemoteExecDisabled
	}
	return b.execRemote(ctx, step)
}
