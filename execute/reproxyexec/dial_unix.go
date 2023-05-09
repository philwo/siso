// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build unix

package reproxyexec

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// DialContext connects to the serverAddress for grpc.
func DialContext(ctx context.Context, serverAddr string) (*grpc.ClientConn, error) {
	return grpc.DialContext(
		ctx,
		serverAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		// Ensure blocking due to flaky reproxy behavior http://tg/639661.
		grpc.WithBlock(),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(grpcMaxMsgSize)))
}
