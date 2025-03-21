// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package semaphore provives semaphore.
package semaphore

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/golang/glog"
)

// Semaphore is a semaphore.
type Semaphore struct {
	name string
	ch   chan int

	waitSpanName string
	servSpanName string

	waits atomic.Int64
	reqs  atomic.Int64
}

// New creates a new semaphore with name and capacity.
func New(name string, n int) *Semaphore {
	ch := make(chan int, n)
	for i := range n {
		ch <- i + 1 // tid
	}
	s := &Semaphore{
		name:         fmt.Sprintf("%s/%d", name, n),
		ch:           ch,
		waitSpanName: fmt.Sprintf("wait:%s/%d", name, n),
		servSpanName: fmt.Sprintf("serv:%s/%d", name, n),
	}
	return s
}

// WaitAcquire acquires a semaphore.
// It returns a context for acquired semaphore and func to release it.
func (s *Semaphore) WaitAcquire(ctx context.Context) (context.Context, func(error), error) {
	s.waits.Add(1)
	defer s.waits.Add(-1)
	now := time.Now()
	select {
	case tid := <-s.ch:
		s.reqs.Add(1)
		if dur := time.Since(now); dur > 1*time.Second {
			glog.Infof("wait %s for %s", s.name, dur)
		}
		return ctx, func(err error) {
			s.ch <- tid
		}, nil
	case <-ctx.Done():
		return ctx, func(error) {}, context.Cause(ctx)
	}
}

var errNotAvailable = errors.New("semaphore: not available")

// TryAcquire acquires a semaphore if available, or return error.
// It returns a context for acquired semaphore and func to release it.
func (s *Semaphore) TryAcquire(ctx context.Context) (context.Context, func(error), error) {
	select {
	case tid := <-s.ch:
		s.reqs.Add(1)
		return ctx, func(err error) {
			s.ch <- tid
		}, nil
	default:
		return ctx, func(error) {}, errNotAvailable
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
