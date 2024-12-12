// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"io"
	"testing"

	"infra/build/siso/build/cachestore"
	"infra/build/siso/reapi/digest"
)

const FirstOnly = "firstonly"
const SecondOnly = "secondonly"
const Both = "both"
const Neither = "neither"
const NewlyAdded = "newly_added"

func makeDigest(s string) digest.Digest {
	return digest.FromBytes(s, []byte(s)).Digest()
}

func setContent(ctx context.Context, cache cachestore.CacheStore, s string) error {
	return cache.SetContent(ctx, makeDigest(s), s, []byte(s))
}

func TestLayeredCache(t *testing.T) {
	ctx := context.Background()
	cache := NewLayeredCache()

	first, err := NewLocalCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := setContent(ctx, first, FirstOnly); err != nil {
		t.Fatal(err)
	}
	if err := setContent(ctx, first, Both); err != nil {
		t.Fatal(err)
	}
	cache.AddLayer(first)

	second, err := NewLocalCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := setContent(ctx, second, SecondOnly); err != nil {
		t.Fatal(err)
	}
	if err := setContent(ctx, second, Both); err != nil {
		t.Fatal(err)
	}

	if err := setContent(ctx, cache, NewlyAdded); err != nil {
		t.Fatal(err)
	}
	cache.AddLayer(second)

	if cache.HasContent(ctx, makeDigest(Neither)) {
		t.Errorf("HasContent(%v) = true, want false", Neither)
	}

	for _, tc := range []string{
		FirstOnly,
		SecondOnly,
		Both,
		NewlyAdded,
	} {
		if !cache.HasContent(ctx, makeDigest(tc)) {
			t.Errorf("HasContent(%v) = false, want true", tc)
		}
		gotContent, err := cache.GetContent(ctx, makeDigest(tc), tc)
		if err != nil {
			t.Error(err)
		}
		if string(gotContent) != tc {
			t.Errorf("GetContent(%v) = %v, want %v", tc, string(gotContent), tc)
		}

		f, err := cache.Source(ctx, makeDigest(tc), tc).Open(ctx)
		if err != nil {
			t.Error(err)
		}
		gotContent, err = io.ReadAll(f)
		f.Close()
		if err != nil {
			t.Error(err)
		}
		if string(gotContent) != tc {
			t.Errorf("io.ReadAll(Source(%v).Open()) = %v, want %v", tc, gotContent, tc)
		}
	}

	if second.HasContent(ctx, makeDigest(FirstOnly)) {
		t.Errorf("second.HasContent(%v) = true, want false", FirstOnly)
	}
	if !first.HasContent(ctx, makeDigest(SecondOnly)) {
		t.Errorf("first.HasContent(%v) = false, want true", SecondOnly)
	}
}
