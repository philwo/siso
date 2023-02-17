// Copyright 2023 The Chromium Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"errors"
	"time"

	"golang.org/x/sync/errgroup"
)

// TODO(b/266518906): move ninjautil.DepsLog to build package and remove this interface.
// DepsLog is an interface to access deps log.
type DepsLog interface {
	// Get gets a deps log by an output with cmdhash.
	// An error is returned when the deps log is not found.
	Get(ctx context.Context, output string, cmdhash []byte) (deps []string, mtime time.Time, err error)

	// Records records a deps log for an output with cmdhash.
	Record(ctx context.Context, output string, cmdhash []byte, mtime time.Time, deps []string) (updaetd bool, err error)
}

// DepsLogs combines local deps log and shared deps log.
type DepsLogs struct {
	Local  DepsLog
	Shared DepsLog
}

// Get returns a local deps log if available. Otherwise, gets a shared deps log.
func (d DepsLogs) Get(ctx context.Context, output string, cmdhash []byte) ([]string, time.Time, error) {
	deps, t, err := d.Local.Get(ctx, output, cmdhash)
	if err == nil {
		return deps, t, nil
	}
	if d.Shared == nil {
		return nil, time.Time{}, errors.New("not found")
	}
	return d.Shared.Get(ctx, output, cmdhash)
}

// Record records the deps log to both local and shared locations.
func (d DepsLogs) Record(ctx context.Context, output string, cmdhash []byte, t time.Time, deps []string) (bool, error) {
	g, ctx := errgroup.WithContext(ctx)

	// updated flags
	var lu, su bool
	g.Go(func() (err error) {
		lu, err = d.Local.Record(ctx, output, cmdhash, t, deps)
		return err
	})
	if d.Shared != nil {
		g.Go(func() (err error) {
			su, err = d.Shared.Record(ctx, output, cmdhash, t, deps)
			return err
		})
	}
	err := g.Wait()
	return lu || su, err
}
