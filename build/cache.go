// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"errors"
	"runtime"
	"time"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	log "github.com/golang/glog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"infra/build/siso/execute"
	"infra/build/siso/o11y/clog"
	"infra/build/siso/o11y/iometrics"
	"infra/build/siso/o11y/trace"
	"infra/build/siso/reapi/digest"
	"infra/build/siso/reapi/merkletree"
	"infra/build/siso/sync/semaphore"
)

// CacheOptions is cache options.
type CacheOptions struct {
	Store      CacheStore
	EnableRead bool
}

// CacheStore is an interface of cache store.
type CacheStore interface {
	// GetActionResult gets the action result of the action identified by the digest.
	GetActionResult(context.Context, digest.Digest) (*rpb.ActionResult, error)
	// GetContent gets the content of the file identified by the digest.
	GetContent(context.Context, digest.Digest, string) ([]byte, error)
	// SetContent sets the content of the file identified by the digest.
	SetContent(context.Context, digest.Digest, string, []byte) error
	// HasContent checks whether content of the digest exists in the cache.
	HasContent(context.Context, digest.Digest) bool

	// DigestData returns digest data for the name identified by the digest.
	DigestData(digest.Digest, string) digest.Data
}

// Cache is a cache used in the builder.
type Cache struct {
	store      CacheStore
	enableRead bool

	sema *semaphore.Semaphore

	m *iometrics.IOMetrics
}

// NewCache creates new cache.
func NewCache(ctx context.Context, opts CacheOptions) (*Cache, error) {
	clog.Infof(ctx, "cache store=%v read=%t",
		opts.Store,
		opts.EnableRead)
	if opts.Store == nil {
		return nil, errors.New("cache: store is not set")
	}
	return &Cache{
		store:      opts.Store,
		enableRead: opts.EnableRead,

		// TODO(b/274038010): cache-digest semaphore should share with execute/remotecache?
		sema: semaphore.New("cache-digest", runtime.NumCPU()*10),

		m: iometrics.New("cache-content"),
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
	ctx, span := trace.NewSpan(ctx, "cache-get")
	defer span.Close(nil)

	var d digest.Digest
	err := c.sema.Do(ctx, func(ctx context.Context) error {
		var err error
		d, err = cmd.Digest(ctx, nil)
		return err
	})
	if err != nil {
		return err
	}
	if !needOutputUpdate(ctx, cmd, d, now) {
		clog.Infof(ctx, "skip: no need to update")
		return nil
	}
	rctx, getSpan := trace.NewSpan(ctx, "get-action-result")
	result, err := c.store.GetActionResult(rctx, d)
	getSpan.Close(nil)
	if err != nil {
		return err
	}
	if log.V(1) {
		clog.Infof(ctx, "cached result: %s", result)
	}

	// copy the action result into cmd.
	cmd.SetActionResult(result)
	c.setActionResultStdout(ctx, cmd, result)
	c.setActionResultStderr(ctx, cmd, result)
	err = cmd.HashFS.Update(ctx, cmd.ExecRoot, cmd.EntriesFromResult(ctx, c.store, result), now, cmd.CmdHash, d)
	clog.Infof(ctx, "cache get %s: %v", time.Since(now), err)
	return err
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

func needOutputUpdate(ctx context.Context, cmd *execute.Cmd, action digest.Digest, now time.Time) bool {
	var entries []merkletree.Entry
	for _, out := range cmd.AllOutputs() {
		fi, err := cmd.HashFS.Stat(ctx, cmd.ExecRoot, out)
		if err != nil {
			clog.Warningf(ctx, "missing output %s: %v", out, err)
			return true
		}
		if fi.Action() != action {
			clog.Infof(ctx, "output action change %s: %v->%v", out, fi.Action(), action)
			return true
		}
		ent, ok := fi.Sys().(merkletree.Entry)
		if !ok {
			clog.Warningf(ctx, "output fileinfo unexpected type %s: %T", out, fi.Sys())
			return true
		}
		entries = append(entries, ent)
	}
	// all outputs were already generated by the previous action. only update mtime.
	err := cmd.HashFS.Update(ctx, cmd.ExecRoot, entries, now, cmd.CmdHash, action)
	if err != nil {
		clog.Warningf(ctx, "failed to update mtime: %v", err)
		return true
	}
	return false
}
