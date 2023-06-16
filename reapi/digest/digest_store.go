// Copyright 2023 The Chromium Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package digest

import "github.com/bazelbuild/remote-apis-sdks/go/pkg/digest"

// Store works as an in-memory content addressable storage.
type Store struct {
	m map[digest.Digest]Data
}

// NewStore creates Store.
func NewStore() *Store {
	return &Store{
		m: make(map[digest.Digest]Data),
	}
}

// Set sets data to the store.
func (s *Store) Set(d Data) {
	s.m[d.Digest()] = d
}

// Get gets data from store by the digest.
func (s *Store) Get(digest digest.Digest) (Data, bool) {
	v, ok := s.m[digest]
	return v, ok
}

// GetSource gets source from the store.
func (s *Store) GetSource(digest digest.Digest) (Source, bool) {
	v, ok := s.Get(digest)
	if !ok {
		return nil, false
	}
	return v.source, true
}

// Size returns the number of digests in the store.
func (s *Store) Size() int {
	if s == nil {
		return 0
	}
	return len(s.m)
}

// List returns a list of the digests of the stored data.
func (s *Store) List() []digest.Digest {
	digests := make([]digest.Digest, 0, len(s.m))
	for k := range s.m {
		digests = append(digests, k)
	}
	return digests
}
