// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.
//
// excerpt from google3/devtools/srcfs/daemon/cog/cog_local_rpc_service.proto#27
// TODO(ukai): update proto once protoc supports edition etc.

// Code generated by protoc-gen-go-grpc. DO NOT EDIT.
// versions:
// - protoc-gen-go-grpc v1.5.1
// - protoc             v5.26.1
// source: local_rpc_service.proto

package proto

import (
	context "context"
	grpc "google.golang.org/grpc"
	codes "google.golang.org/grpc/codes"
	status "google.golang.org/grpc/status"
)

// This is a compile-time assertion to ensure that this generated file
// is compatible with the grpc package it is being compiled against.
// Requires gRPC-Go v1.64.0 or later.
const _ = grpc.SupportPackageIsVersion9

const (
	CogLocalRpcService_BuildfsInsert_FullMethodName = "/devtools_srcfs.CogLocalRpcService/BuildfsInsert"
)

// CogLocalRpcServiceClient is the client API for CogLocalRpcService service.
//
// For semantics around ctx use and closing/ending streaming RPCs, please refer to https://pkg.go.dev/google.golang.org/grpc/?tab=doc#ClientConn.NewStream.
type CogLocalRpcServiceClient interface {
	BuildfsInsert(ctx context.Context, in *BuildfsInsertRequest, opts ...grpc.CallOption) (*BuildfsInsertResponse, error)
}

type cogLocalRpcServiceClient struct {
	cc grpc.ClientConnInterface
}

func NewCogLocalRpcServiceClient(cc grpc.ClientConnInterface) CogLocalRpcServiceClient {
	return &cogLocalRpcServiceClient{cc}
}

func (c *cogLocalRpcServiceClient) BuildfsInsert(ctx context.Context, in *BuildfsInsertRequest, opts ...grpc.CallOption) (*BuildfsInsertResponse, error) {
	cOpts := append([]grpc.CallOption{grpc.StaticMethod()}, opts...)
	out := new(BuildfsInsertResponse)
	err := c.cc.Invoke(ctx, CogLocalRpcService_BuildfsInsert_FullMethodName, in, out, cOpts...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// CogLocalRpcServiceServer is the server API for CogLocalRpcService service.
// All implementations must embed UnimplementedCogLocalRpcServiceServer
// for forward compatibility.
type CogLocalRpcServiceServer interface {
	BuildfsInsert(context.Context, *BuildfsInsertRequest) (*BuildfsInsertResponse, error)
	mustEmbedUnimplementedCogLocalRpcServiceServer()
}

// UnimplementedCogLocalRpcServiceServer must be embedded to have
// forward compatible implementations.
//
// NOTE: this should be embedded by value instead of pointer to avoid a nil
// pointer dereference when methods are called.
type UnimplementedCogLocalRpcServiceServer struct{}

func (UnimplementedCogLocalRpcServiceServer) BuildfsInsert(context.Context, *BuildfsInsertRequest) (*BuildfsInsertResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method BuildfsInsert not implemented")
}
func (UnimplementedCogLocalRpcServiceServer) mustEmbedUnimplementedCogLocalRpcServiceServer() {}
func (UnimplementedCogLocalRpcServiceServer) testEmbeddedByValue()                            {}

// UnsafeCogLocalRpcServiceServer may be embedded to opt out of forward compatibility for this service.
// Use of this interface is not recommended, as added methods to CogLocalRpcServiceServer will
// result in compilation errors.
type UnsafeCogLocalRpcServiceServer interface {
	mustEmbedUnimplementedCogLocalRpcServiceServer()
}

func RegisterCogLocalRpcServiceServer(s grpc.ServiceRegistrar, srv CogLocalRpcServiceServer) {
	// If the following call pancis, it indicates UnimplementedCogLocalRpcServiceServer was
	// embedded by pointer and is nil.  This will cause panics if an
	// unimplemented method is ever invoked, so we test this at initialization
	// time to prevent it from happening at runtime later due to I/O.
	if t, ok := srv.(interface{ testEmbeddedByValue() }); ok {
		t.testEmbeddedByValue()
	}
	s.RegisterService(&CogLocalRpcService_ServiceDesc, srv)
}

func _CogLocalRpcService_BuildfsInsert_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(BuildfsInsertRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(CogLocalRpcServiceServer).BuildfsInsert(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: CogLocalRpcService_BuildfsInsert_FullMethodName,
	}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(CogLocalRpcServiceServer).BuildfsInsert(ctx, req.(*BuildfsInsertRequest))
	}
	return interceptor(ctx, in, info, handler)
}

// CogLocalRpcService_ServiceDesc is the grpc.ServiceDesc for CogLocalRpcService service.
// It's only intended for direct use with grpc.RegisterService,
// and not to be introspected or modified (even as a copy)
var CogLocalRpcService_ServiceDesc = grpc.ServiceDesc{
	ServiceName: "devtools_srcfs.CogLocalRpcService",
	HandlerType: (*CogLocalRpcServiceServer)(nil),
	Methods: []grpc.MethodDesc{
		{
			MethodName: "BuildfsInsert",
			Handler:    _CogLocalRpcService_BuildfsInsert_Handler,
		},
	},
	Streams:  []grpc.StreamDesc{},
	Metadata: "local_rpc_service.proto",
}
