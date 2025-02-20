// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package retry_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"go.chromium.org/luci/common/clock"
	"go.chromium.org/luci/common/clock/testclock"

	"go.chromium.org/infra/build/siso/reapi/retry"
)

func TestDo(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	t.Run("no retry", func(t *testing.T) {
		called := 0
		err := retry.Do(ctx, func() error {
			called++
			return nil
		})
		if err != nil {
			t.Errorf("want nil, got err: %v", err)
		}
		if called != 1 {
			t.Errorf("want 1, got %d", called)
		}
	})

	t.Run("non-retriable error", func(t *testing.T) {
		called := 0
		testErr := fmt.Errorf("error")
		err := retry.Do(ctx, func() error {
			called++
			return testErr
		})
		if err != testErr {
			t.Errorf("want testErr, got err: %v", err)
		}
		if called != 1 {
			t.Errorf("want 1, got %d", called)
		}
	})

	t.Run("retriable error", func(t *testing.T) {
		ctx, c := testclock.UseTime(ctx, time.Now())
		c.SetTimerCallback(func(time.Duration, clock.Timer) {
			c.Add(time.Second)
		})

		called := 0
		err := retry.Do(ctx, func() error {
			called++
			if called == 1 {
				return status.Error(codes.Internal, "retriable error")
			}
			return nil
		})
		if err != nil {
			t.Errorf("want nil, got err: %v", err)
		}
		if called != 2 {
			t.Errorf("want 2, got %d", called)
		}
	})
}
