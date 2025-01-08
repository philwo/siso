// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"errors"
	"time"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	log "github.com/golang/glog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"infra/build/siso/build/cachestore"
	"infra/build/siso/execute"
	"infra/build/siso/o11y/clog"
	"infra/build/siso/o11y/iometrics"
	"infra/build/siso/o11y/trace"
	"infra/build/siso/reapi/digest"
	"infra/build/siso/runtimex"
	"infra/build/siso/sync/semaphore"
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
		sema: semaphore.New("cache-digest", runtimex.NumCPU()*10),

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
	cmd.SetActionDigest(d)
	cmd.SetActionResult(result, true)
	c.setActionResultStdout(ctx, cmd, result)
	c.setActionResultStderr(ctx, cmd, result)
	err = cmd.RecordOutputs(ctx, c.store, now)
	if err != nil {
		clog.Infof(ctx, "cache get %s: %v", time.Since(now), err)
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

// needOutputUpdate reports the action needs to update outputs,
// i.e. outputs doesn't exist or not generated by the same action digest,
// or failed to update mtime.
func needOutputUpdate(ctx context.Context, cmd *execute.Cmd, action digest.Digest, now time.Time) bool {
	// TODO: rewrite using RetrieveUpdateEntries and check cmdhash/action?
	checkSameAction := func(out string) bool {
		fi, err := cmd.HashFS.Stat(ctx, cmd.ExecRoot, out)
		if err != nil {
			if log.V(1) {
				clog.Infof(ctx, "missing output %s: %v", out, err)
			}
			return true
		}
		if fi.Action() != action {
			clog.Infof(ctx, "output action change %s: %v->%v", out, fi.Action(), action)
			return true
		}
		return false
	}
	updateMtime := func(outs []string, cmdhash []byte) bool {
		entries := cmd.HashFS.RetrieveUpdateEntries(ctx, cmd.ExecRoot, outs)
		// all outputs were already generated by the previous action. only update mtime.
		for i, ent := range entries {
			ent.ModTime = now
			ent.CmdHash = cmdhash
			ent.Action = action
			ent.UpdatedTime = now
			ent.IsChanged = true
			entries[i] = ent
		}
		err := cmd.HashFS.Update(ctx, cmd.ExecRoot, entries)
		if err != nil {
			clog.Warningf(ctx, "failed to update mtime: %v", err)
			return true
		}
		return false
	}

	var outs []string
	for _, out := range cmd.Outputs {
		if checkSameAction(out) {
			return true
		}
		outs = append(outs, out)
	}
	if updateMtime(outs, cmd.CmdHash) {
		return true
	}
	if cmd.Depfile == "" {
		return false
	}
	switch cmd.Deps {
	case "gcc", "msvc":
		// deps=gcc, msvc loads deps into deps log and remove,
		// so no need to check depfile.
		return false
	}
	// check depfile is up-to-date or not (i.e. need to update)
	out := cmd.Depfile
	if checkSameAction(out) {
		return true
	}
	return updateMtime([]string{out}, nil)
}
