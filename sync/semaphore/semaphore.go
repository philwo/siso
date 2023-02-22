// Copyright 2023 The Chromium Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// TODO(b/267409605): add test.

// Package semaphore provives semaphore.
package semaphore

import (
	"context"
	"sync"
	"sync/atomic"
)

var (
	mu         sync.Mutex
	semaphores = map[string]*Semaphore{}
)

// Semaphore is a semaphore.
type Semaphore struct {
	name string
	ch   chan int

	waits atomic.Int64
	reqs  atomic.Int64
}

// Lookup returns a semaphore for the name, or nil if not registered.
// TODO(jwata): return error for missing semaphore.
func Lookup(name string) *Semaphore {
	mu.Lock()
	defer mu.Unlock()
	return semaphores[name]
}

// New creates a new semaphore with name and capacity.
func New(name string, n int) *Semaphore {
	ch := make(chan int, n)
	for i := 0; i < n; i++ {
		ch <- i + 1 // tid
	}
	s := &Semaphore{
		name: name,
		ch:   ch,
	}
	mu.Lock()
	semaphores[name] = s
	mu.Unlock()
	return s
}

// WaitAcquire acquires a semaphore.
// It returns a context for acquired semaphore and func to release it.
// TODO(b/267576561): add Cloud Trace integration and add tid as an attribute of a Span.
func (s *Semaphore) WaitAcquire(ctx context.Context) (context.Context, func(), error) {
	s.waits.Add(1)
	defer s.waits.Add(-1)
	select {
	case tid := <-s.ch:
		s.reqs.Add(1)
		return ctx, func() {
			s.ch <- tid
		}, nil
	case <-ctx.Done():
		return ctx, func() {}, ctx.Err()
	}
}

// Name returns name of the semaphore.
func (s *Semaphore) Name() string {
	return s.name
}

// Capacity returns capacity of the semaphore.
func (s *Semaphore) Capacity() int {
	if s == nil {
		return 0
	}
	return cap(s.ch)
}

// NumServes returns number of currently served.
func (s *Semaphore) NumServs() int {
	return cap(s.ch) - len(s.ch)
}

// NumWaits returns number of waiters.
func (s *Semaphore) NumWaits() int {
	return int(s.waits.Load())
}

// NumRequests returns total number of requests.
func (s *Semaphore) NumRequests() int {
	return int(s.reqs.Load())
}

// Do runs f under semaphore.
func (s *Semaphore) Do(ctx context.Context, f func(ctx context.Context) error) error {
	ctx, done, err := s.WaitAcquire(ctx)
	if err != nil {
		return err
	}
	defer done()
	return f(ctx)
}
