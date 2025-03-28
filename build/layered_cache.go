// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"infra/build/siso/build/cachestore"
	"infra/build/siso/o11y/clog"
	"infra/build/siso/reapi/digest"
)

// LayeredCache is a multi-layer cache. It will attempt to read from caches in
// priority order, and when doing so, performs write-through caching to all
// faster caches.
// When writing, it only writes to the first layer to improve performance.
type LayeredCache struct {
	// The caches here are ordered by priority (higher priority first).
	caches []cachestore.CacheStore
}

func NewLayeredCache() *LayeredCache {
	return &LayeredCache{}
}

// AddLayer adds a layer to the cache.
// layered cache with an extra layer attached.
func (lc *LayeredCache) AddLayer(cache cachestore.CacheStore) {
	lc.caches = append(lc.caches, cache)
}

func isNotExist(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || status.Code(err) == codes.NotFound
}

// GetActionResult gets the action result of the action identified by the digest.
func (lc *LayeredCache) GetActionResult(ctx context.Context, d digest.Digest) (ar *rpb.ActionResult, err error) {
	for _, cache := range lc.caches {
		ar, err = cache.GetActionResult(ctx, d)
		if err == nil || !isNotExist(err) {
			return ar, err
		}
	}
	return nil, fmt.Errorf("no caches to retrieve content from")
}

// SetActionResult sets the action result of the action identified by the digest.
// If a failing action is provided, caching will be skipped.
func (lc *LayeredCache) SetActionResult(ctx context.Context, d digest.Digest, ar *rpb.ActionResult) error {
	// Intentionally only write to the first layer of the cache to improve
	// performance.
	// See SetContent for rationale.
	if len(lc.caches) > 0 {
		if err := lc.caches[0].SetActionResult(ctx, d, ar); err != nil {
			return err
		}
	}
	return nil
}

// GetContent gets the content of the file identified by the digest.
func (lc *LayeredCache) GetContent(ctx context.Context, d digest.Digest, f string) (content []byte, err error) {
	for i, cache := range lc.caches {
		content, err = cache.GetContent(ctx, d, f)
		if isNotExist(err) {
			continue
		}
		if err != nil {
			return content, err
		}
		// If it exists in a slow cache, write it to all faster caches.
		for j := range i {
			if err := lc.caches[j].SetContent(ctx, d, f, content); err != nil {
				clog.Warningf(ctx, "failed to write digest %s to cache: %v", d.String(), err)
			}
		}
		return content, err
	}
	return nil, fmt.Errorf("no caches to retrieve content from")
}

// SetContent sets the content of the file identified by the digest.
func (lc *LayeredCache) SetContent(ctx context.Context, d digest.Digest, f string, content []byte) error {
	// Intentionally only write to the first layer of the cache.
	// Pro: More performant because no slow cache uploads.
	// Con: No shared remote cache for pure local actions. However, that doesn't
	//   matter too much, since it'd only be useful if the entry was purged from
	//   your local CAS but still existed in your local ActionResult cache.
	if len(lc.caches) > 0 {
		if err := lc.caches[0].SetContent(ctx, d, f, content); err != nil {
			return err
		}
	}
	return nil
}

// HasContent checks whether content of the digest exists in the cache.
func (lc *LayeredCache) HasContent(ctx context.Context, d digest.Digest) bool {
	for _, cache := range lc.caches {
		if cache.HasContent(ctx, d) {
			return true
		}
	}
	return false
}

// Source returns digest source for the name identified by the digest.
func (lc *LayeredCache) Source(ctx context.Context, d digest.Digest, f string) digest.Source {
	if len(lc.caches) == 0 {
		return nil
	}
	for _, cache := range lc.caches[:len(lc.caches)-1] {
		if cache.HasContent(ctx, d) {
			return cache.Source(ctx, d, f)
		}
	}
	return lc.caches[len(lc.caches)-1].Source(ctx, d, f)
}
