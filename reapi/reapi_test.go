// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package reapi

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
)

func TestServiceConfig_CAS(t *testing.T) {
	for _, tc := range []struct {
		code     codes.Code
		n        int
		wantCode codes.Code
	}{
		{
			code:     codes.OK,
			n:        0,
			wantCode: codes.OK,
		},
		{
			code:     codes.Unavailable,
			n:        1,
			wantCode: codes.OK,
		},
		{
			code:     codes.ResourceExhausted,
			n:        1,
			wantCode: codes.OK,
		},
		{
			code:     codes.Unavailable,
			n:        6,
			wantCode: codes.Unavailable,
		},
		{
			code:     codes.ResourceExhausted,
			n:        6,
			wantCode: codes.ResourceExhausted,
		},
		{
			code:     codes.PermissionDenied,
			n:        1,
			wantCode: codes.PermissionDenied,
		},
		{
			code:     codes.Internal,
			n:        1,
			wantCode: codes.Internal,
		},
	} {
		t.Run(fmt.Sprintf("%s_%d", tc.code, tc.n), func(t *testing.T) {
			ctx := context.Background()
			ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
			defer cancel()

			ch := make(chan struct{})
			lis, err := net.Listen("tcp", "localhost:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				err = lis.Close()
				if err != nil {
					t.Error(err)
				}
				<-ch
			})
			addr := lis.Addr().String()
			t.Logf("fake addr: %s", addr)
			serv := grpc.NewServer()
			cas := &fakeCAS{
				t:    t,
				code: tc.code,
				n:    tc.n,
			}
			rpb.RegisterContentAddressableStorageServer(serv, cas)
			reflection.Register(serv)
			go func() {
				defer close(ch)
				err := serv.Serve(lis)
				t.Logf("serve finished: %v", err)
			}()

			keepAliveParams := keepalive.ClientParameters{
				Time:    30 * time.Second,
				Timeout: 20 * time.Second,
			}
			conn, err := grpc.DialContext(ctx, addr, append([]grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}, dialOptions(keepAliveParams)...)...)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				err := conn.Close()
				if err != nil {
					t.Errorf("conn close=%v", err)
				}
			}()
			t.Logf("dial done")
			client := rpb.NewContentAddressableStorageClient(conn)
			_, err = client.BatchUpdateBlobs(ctx, &rpb.BatchUpdateBlobsRequest{})
			if status.Code(err) != tc.wantCode {
				t.Errorf("BatchUpdateBlobs=%v; want %v", err, tc.wantCode)
			}
		})
	}
}

type fakeCAS struct {
	rpb.UnimplementedContentAddressableStorageServer
	t    *testing.T
	code codes.Code
	// remaining number to reply error with code.
	n int
}

func (f *fakeCAS) BatchUpdateBlobs(ctx context.Context, req *rpb.BatchUpdateBlobsRequest) (*rpb.BatchUpdateBlobsResponse, error) {
	n := f.n
	f.n--
	var err error
	if n > 0 {
		err = status.Error(f.code, "error")
	}
	f.t.Logf("n=%d: err=%v", n, err)
	return &rpb.BatchUpdateBlobsResponse{}, err
}
