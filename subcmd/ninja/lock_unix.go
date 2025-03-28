// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build unix

package ninja

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

type lockFile struct {
	f *os.File
}

func newLockFile(ctx context.Context, fname string) (*lockFile, error) {
	f, err := os.OpenFile(fname, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return nil, err
	}
	return &lockFile{f: f}, nil
}

func (l *lockFile) Close() error {
	return l.f.Close()
}

func (l *lockFile) Lock() error {
	err := unix.Flock(int(l.f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			_, _ = l.f.Seek(0, io.SeekStart)
			buf, err := io.ReadAll(l.f)
			if err != nil {
				return fmt.Errorf("%s is locked, and failed to read: %w", l.f.Name(), err)
			}
			return fmt.Errorf("%s is locked by %s: %w", l.f.Name(), string(buf), unix.EWOULDBLOCK)
		}
		return err
	}
	fmt.Fprintf(l.f, "pid=%d", os.Getpid())
	return nil
}

func (l *lockFile) Unlock() error {
	return unix.Flock(int(l.f.Fd()), unix.LOCK_UN)
}
