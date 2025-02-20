// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package retry provides retrying functionalities.
package retry

import (
	"context"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"go.chromium.org/luci/common/errors"
	"go.chromium.org/luci/common/retry"
	"go.chromium.org/luci/common/retry/transient"

	"go.chromium.org/infra/build/siso/o11y/clog"
)

func retriableError(err error, authRetry *int) bool {
	st, ok := status.FromError(err)
	if !ok {
		st = status.FromContextError(err)
	}

	// https://github.com/bazelbuild/bazel/blob/7.1.1/src/main/java/com/google/devtools/build/lib/remote/RemoteRetrier.java#L47
	switch st.Code() {
	case codes.ResourceExhausted,
		codes.Internal,
		codes.Unavailable,
		codes.Aborted:
		return true
	case codes.Unknown:
		// unknown grpc error should retry, but non grpc error should not.
		return ok
	case
		// may get
		// code = Unauthenticated desc = Request had invalid authentication credentials.
		// Expected OAuth 2 access token, login cookie or other valid authentication credential.
		// See https://developers.google.com/identity/sign-in/web/devconsole-project.
		// (access token expired, need to refresh).
		// or
		// code = PermissionDenied desc = The caller does not have permission
		//
		// but should not retry (wrong auth, instance without permission)
		// code = PermissionDenied desc = Permission "xx" denied on resource "yy" (or it may not exist)
		// code = PermissionDenied desc = Permission denied on resource project xx
		// code = PermissionDenied desc = Remote Build Execution API has not been used in project rbe-android before or it is disabled. Enable it by visiting https://console.developers.google.com/apis/api/remotebuildexecution.googleapis.com/overview?project=rbe-android then retry. If you enabled this API recently, wait a few minutes for the action to propagate to our systems and retry.
		codes.Unauthenticated,
		codes.PermissionDenied:
		// Allow authRetry at most once.
		// Next call should not fail with auth error if it was
		// token expired.
		// It does not make sense to retry more if it is lack of
		// permission or so.
		*authRetry++
		return *authRetry < 2
	}
	return false
}

// Do calls function `f` and retries with exponential backoff for errors that are known to be retriable.
func Do(ctx context.Context, f func() error) error {
	authRetry := 0
	return retry.Retry(ctx, transient.Only(retry.Default), func() error {
		err := f()
		if retriableError(err, &authRetry) {
			return errors.Annotate(err, "retriable error").Tag(transient.Tag).Err()
		}
		return err
	}, func(err error, backoff time.Duration) {
		clog.Warningf(ctx, "retry backoff:%s: %v", backoff, err)
	})
}
