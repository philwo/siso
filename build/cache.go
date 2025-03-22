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

	"go.chromium.org/infra/build/siso/build/cachestore"
	"go.chromium.org/infra/build/siso/execute"
	"go.chromium.org/infra/build/siso/reapi/digest"
	"go.chromium.org/infra/build/siso/runtimex"
	"go.chromium.org/infra/build/siso/sync/semaphore"
)

// CacheOptions is cache options.
type CacheOptions struct {
	Store      cachestore.CacheStore
	EnableRead bool
}

// Cache is a cache used in the builder.
type Cache struct {
	store      cachestore.CacheStore
	enableRead bool

	sema *semaphore.Semaphore
}

// NewCache creates new cache.
func NewCache(opts CacheOptions) (*Cache, error) {
	log.Infof("cache store=%v read=%t",
		opts.Store,
		opts.EnableRead)
	if opts.Store == nil {
		return nil, errors.New("cache: store is not set")
	}
	return &Cache{
		store:      opts.Store,
		enableRead: opts.EnableRead,

		// TODO(b/274038010): cache-digest semaphore should share with execute/remotecache?
		sema: semaphore.New("cache-digest", runtimex.NumCPU()*10),
	}, nil
}

// GetActionResult gets action result for the cmd from cache.
func (c *Cache) GetActionResult(ctx context.Context, cmd *execute.Cmd) error {
	now := time.Now()
	if c == nil || c.store == nil {
		return status.Error(codes.NotFound, "cache is not configured")
	}
	if !c.enableRead {
		return status.Error(codes.NotFound, "cache disable raed")
	}

	var d digest.Digest
	err := c.sema.Do(ctx, func(ctx context.Context) error {
		var err error
		d, err = cmd.Digest(ctx, nil)
		return err
	})
	if err != nil {
		return err
	}
	result, err := c.store.GetActionResult(ctx, d)
	if err != nil {
		return err
	}

	// copy the action result into cmd.
	cmd.SetActionDigest(d)
	cmd.SetActionResult(result, true)
	c.setActionResultStdout(ctx, cmd, result)
	c.setActionResultStderr(ctx, cmd, result)
	err = cmd.RecordOutputs(ctx, c.store, now)
	if err != nil {
		log.Infof("cache get %s: %v", time.Since(now), err)
		return err
	}
	return nil
}

func (c *Cache) setActionResultStdout(ctx context.Context, cmd *execute.Cmd, result *rpb.ActionResult) {
	w := cmd.StdoutWriter()
	if len(result.StdoutRaw) > 0 {
		w.Write(result.StdoutRaw)
		return
	}
	d := digest.FromProto(result.GetStdoutDigest())
	if d.SizeBytes == 0 {
		return
	}
	buf, err := c.store.GetContent(ctx, d, "stdout")
	if err != nil {
		w.Write([]byte(err.Error()))
	}
	w.Write(buf)
}

func (c *Cache) setActionResultStderr(ctx context.Context, cmd *execute.Cmd, result *rpb.ActionResult) {
	w := cmd.StderrWriter()
	if len(result.StderrRaw) > 0 {
		w.Write(result.StderrRaw)
		return
	}
	d := digest.FromProto(result.GetStderrDigest())
	if d.SizeBytes == 0 {
		return
	}
	buf, err := c.store.GetContent(ctx, d, "stderr")
	if err != nil {
		w.Write([]byte(err.Error()))
	}
	w.Write(buf)
}
