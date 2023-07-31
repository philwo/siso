// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package hashfs

import (
	"context"
	"runtime"
	"sync"

	"github.com/pkg/xattr"

	"infra/build/siso/o11y/clog"
	"infra/build/siso/o11y/iometrics"
	"infra/build/siso/reapi/digest"
	"infra/build/siso/sync/semaphore"
)

// DigestSemaphore is a semaphore to control concurrent digest calculation.
var DigestSemaphore = semaphore.New("file-digest", runtime.NumCPU())

func localDigest(ctx context.Context, fname, xattrname string, size int64, m *iometrics.IOMetrics) (digest.Data, error) {
	src := digest.LocalFileSource{Fname: fname, IOMetrics: m}
	if xattrname != "" {
		d, err := xattr.LGet(fname, xattrname)
		if err == nil {
			return digest.NewData(src, digest.Digest{
				Hash:      string(d),
				SizeBytes: size,
			}), nil
		}
	}
	return digest.FromLocalFile(ctx, src)
}

type digestReq struct {
	ctx   context.Context
	fname string
	e     *entry
}

type digester struct {
	xattrname string
	q         chan digestReq

	mu    sync.Mutex
	queue []digestReq

	quit chan struct{}
	done chan struct{}
}

func (d *digester) start() {
	defer close(d.done)
	n := runtime.NumCPU() - 1
	if n == 0 {
		n = 1
	}
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.worker()
		}()
	}
	wg.Wait()
}

func (d *digester) worker() {
	for {
		select {
		case <-d.quit:
			return
		case req := <-d.q:
			d.compute(req.ctx, req.fname, req.e)
			d.mu.Lock()
			if len(d.queue) > 0 {
				select {
				case d.q <- d.queue[0]:
					copy(d.queue, d.queue[:len(d.queue)-1])
					d.queue[len(d.queue)-1] = digestReq{}
					d.queue = d.queue[:len(d.queue)-1]
				default:
				}
			}
			d.mu.Unlock()
		}
	}
}

func (d *digester) stop() {
	close(d.quit)
	<-d.done
}

func (d *digester) lazyCompute(ctx context.Context, fname string, e *entry) {
	req := digestReq{
		ctx:   ctx,
		fname: fname,
		e:     e,
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	select {
	case d.q <- req:
	default:
		d.queue = append(d.queue, req)
	}
}

func (d *digester) compute(ctx context.Context, fname string, e *entry) {
	err := DigestSemaphore.Do(ctx, func(ctx context.Context) error {
		return e.compute(ctx, fname, d.xattrname)
	})
	if err != nil {
		clog.Warningf(ctx, "failed to digest compute %s: %v", fname, err)
	}
}
