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
	"time"

	"github.com/charmbracelet/log"
)

func (b *Builder) setupRSP(ctx context.Context, step *Step) error {
	rsp := step.cmd.RSPFile
	if rsp == "" {
		return nil
	}
	content := step.cmd.RSPFileContent
	err := b.hashFS.WriteFile(ctx, step.cmd.ExecRoot, rsp, content, false, time.Now(), nil, nil)
	if err != nil {
		return fmt.Errorf("failed to create rsp %s: %w", rsp, err)
	}
	return nil
}

func (b *Builder) teardownRSP(ctx context.Context, step *Step) {
	if b.keepRSP {
		return
	}
	rsp := step.cmd.RSPFile
	if rsp == "" {
		return
	}
	log.Debugf("remove rsp %q", rsp)
	err := b.hashFS.Remove(ctx, step.cmd.ExecRoot, rsp)
	if err != nil {
		log.Warnf("failed to remove %s: %v", rsp, err)
	}
	// remove local file if it is used on local?
	err = os.Remove(filepath.Join(step.cmd.ExecRoot, rsp))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Warnf("failed to remove %s: %v", rsp, err)
	}
}
