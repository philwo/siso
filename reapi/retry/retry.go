// Copyright 2023 The Chromium Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package retry provides retrying functionalities.
package retry

import (
	"context"

	"go.chromium.org/luci/common/errors"
	"go.chromium.org/luci/common/retry"
	"go.chromium.org/luci/common/retry/transient"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func retriableError(err error, called int) bool {
	st, ok := status.FromError(err)
	if !ok {
		st = status.FromContextError(err)
	}

	switch st.Code() {
	case codes.ResourceExhausted,
		codes.Internal,
		codes.Unavailable:
		return true
	case
		// may get
		// code = Unauthenticated desc = Request had invalid authentication credentials.
		// Expected OAuth 2 access token, login cookie or other valid authentication credential.
		// See https://developers.google.com/identity/sign-in/web/devconsole-project.
		// (access token expired, need to refresh).
		// but should not retry if it gets in the first request (wrong auth?)
		codes.Unauthenticated:
		return called != 1
	}
	return false
}

// Do calls function `f` and retries with exponential backoff for errors that are known to be retriable.
func Do(ctx context.Context, f func() error) error {
	called := 0
	return retry.Retry(ctx, transient.Only(retry.Default), func() error {
		called++
		err := f()
		if retriableError(err, called) {
			return errors.Annotate(err, "retriable error").Tag(transient.Tag).Err()
		}
		return err
	}, nil)
}
