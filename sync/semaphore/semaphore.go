// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// TODO(b/267409605): add test.

// Package semaphore provives semaphore.
package semaphore

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"google.golang.org/grpc/status"

	"infra/build/siso/o11y/trace"
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

// Lookup returns a semaphore for the name, or error if not registered.
func Lookup(name string) (*Semaphore, error) {
	mu.Lock()
	defer mu.Unlock()
	s, ok := semaphores[name]
	if !ok {
		return nil, fmt.Errorf("semaphore %q does not exist", name)
	}
	return s, nil
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
func (s *Semaphore) WaitAcquire(ctx context.Context) (context.Context, func(error), error) {
	_, span := trace.NewSpan(ctx, fmt.Sprintf("wait:%s", s.name))
	s.waits.Add(1)
	defer span.Close(nil)
	defer s.waits.Add(-1)
	select {
	case tid := <-s.ch:
		s.reqs.Add(1)
		ctx, span := trace.NewSpan(ctx, fmt.Sprintf("serv:%s", s.name))
		span.SetAttr("tid", tid)
		return ctx, func(err error) {
			st, ok := status.FromError(err)
			if !ok {
				st = status.FromContextError(err)
			}
			span.Close(st.Proto())
			s.ch <- tid
		}, nil
	case <-ctx.Done():
		return ctx, func(error) {}, context.Cause(ctx)
	}
}

// Name returns name of the semaphore.
func (s *Semaphore) Name() string {
	return s.name
}

// Capacity returns capacity of the semaphore.
func (s *Semaphore) Capacity() int {
	return cap(s.ch)
}

// NumServs returns number of currently served.
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
func (s *Semaphore) Do(ctx context.Context, f func(ctx context.Context) error) (err error) {
	var done func(error)
	ctx, done, err = s.WaitAcquire(ctx)
	if err != nil {
		return err
	}
	defer func() { done(err) }()
	err = f(ctx)
	return err
}
