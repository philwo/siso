// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build windows

package ninja

import (
	"context"
	"errors"
)

// TODO: support lock file on windows

type lockFile struct{}

func newLockFile(ctx context.Context, fname string) (*lockFile, error) {
	return nil, errors.ErrUnsupported
}

func (l *lockFile) Close() error  { return errors.ErrUnsupported }
func (l *lockFile) Lock() error   { return errors.ErrUnsupported }
func (l *lockFile) Unlock() error { return errors.ErrUnsupported }
