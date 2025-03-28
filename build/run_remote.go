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

// runRemote runs step with using remote apis.
func (b *Builder) runRemote(ctx context.Context, step *Step) error {
	cacheCheck := b.cache != nil && b.reCacheEnableRead
	step.setPhase(stepPreproc)
	err := depsCmd(ctx, b, step)
	if err != nil {
		// disable remote execution. b/289143861
		step.cmd.Platform = nil
		return fmt.Errorf("disable remote: failed to get %s deps: %w", step.cmd.Deps, err)
	}
	dedupInputs(step.cmd)
	err = b.runRemoteStep(ctx, step, cacheCheck)
	if err != nil {
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
	if len(step.cmd.Platform) == 0 || step.cmd.Platform["container-image"] == "" {
		return fmt.Errorf("no remote available (missing container-image property)")
	}
	if cacheCheck {
		err := b.execRemoteCache(ctx, step)
		if err == nil {
			return nil
		}
	}
	return b.execRemote(ctx, step)
}
