// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build windows

package reproxyexec

import (
	"context"
	"net"
	"strings"

	winio "github.com/Microsoft/go-winio"
	"google.golang.org/grpc"
)

// DialContext connects to the serverAddress for grpc.
// if serverAddr is `pipe://<addr>`, it connects to named pipe (`\\.\\pipe\<addr>`).
func DialContext(ctx context.Context, serverAddr string) (*grpc.ClientConn, error) {
	if strings.HasPrefix(serverAddr, "pipe://") {
		return dialPipe(ctx, strings.TrimPrefix(serverAddr, "pipe://"))
	}
	// Although grpc.DialContxt is deprecated, but grpc.NewClient
	// doesn't have blocking feature.
	// https://crrev.com/c/5642324 didn't work well?
	// https://ci.chromium.org/ui/p/chromium/builders/build/win-build-perf-developer/1232/overview
	return grpc.DialContext(
		ctx,
		serverAddr,
		grpc.WithInsecure(),
		// Ensure blocking due to flaky reproxy behavior http://tg/639661.
		grpc.WithBlock(),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(grpcMaxMsgSize)))
}

func dialPipe(ctx context.Context, pipe string) (*grpc.ClientConn, error) {
	addr := `\\.\pipe\` + pipe
	return grpc.DialContext(
		ctx,
		addr,
		grpc.WithInsecure(),
		// Ensure blocking due to flaky reproxy behavior http://tg/639661.
		grpc.WithBlock(),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(grpcMaxMsgSize)),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return winio.DialPipeContext(ctx, addr)
		}))
}
