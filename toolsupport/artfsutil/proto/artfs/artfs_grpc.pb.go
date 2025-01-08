// Code generated by protoc-gen-go-grpc. DO NOT EDIT.
// versions:
// - protoc-gen-go-grpc v1.5.1
// - protoc             v3.21.12
// source: artfs.proto

package artfs

import (
	context "context"
	grpc "google.golang.org/grpc"
	codes "google.golang.org/grpc/codes"
	status "google.golang.org/grpc/status"
	manifest "infra/build/siso/toolsupport/artfsutil/proto/manifest"
)

// This is a compile-time assertion to ensure that this generated file
// is compatible with the grpc package it is being compiled against.
// Requires gRPC-Go v1.64.0 or later.
const _ = grpc.SupportPackageIsVersion9

const (
	Artfs_AddCasFiles_FullMethodName = "/artfs.Artfs/AddCasFiles"
)

// ArtfsClient is the client API for Artfs service.
//
// For semantics around ctx use and closing/ending streaming RPCs, please refer to https://pkg.go.dev/google.golang.org/grpc/?tab=doc#ClientConn.NewStream.
type ArtfsClient interface {
	// Stream a bunch of files that are backed by CAS to Artfs.
	AddCasFiles(ctx context.Context, opts ...grpc.CallOption) (grpc.ClientStreamingClient[manifest.FileManifest, AddCasFilesReply], error)
}

type artfsClient struct {
	cc grpc.ClientConnInterface
}

func NewArtfsClient(cc grpc.ClientConnInterface) ArtfsClient {
	return &artfsClient{cc}
}

func (c *artfsClient) AddCasFiles(ctx context.Context, opts ...grpc.CallOption) (grpc.ClientStreamingClient[manifest.FileManifest, AddCasFilesReply], error) {
	cOpts := append([]grpc.CallOption{grpc.StaticMethod()}, opts...)
	stream, err := c.cc.NewStream(ctx, &Artfs_ServiceDesc.Streams[0], Artfs_AddCasFiles_FullMethodName, cOpts...)
	if err != nil {
		return nil, err
	}
	x := &grpc.GenericClientStream[manifest.FileManifest, AddCasFilesReply]{ClientStream: stream}
	return x, nil
}

// This type alias is provided for backwards compatibility with existing code that references the prior non-generic stream type by name.
type Artfs_AddCasFilesClient = grpc.ClientStreamingClient[manifest.FileManifest, AddCasFilesReply]

// ArtfsServer is the server API for Artfs service.
// All implementations must embed UnimplementedArtfsServer
// for forward compatibility.
type ArtfsServer interface {
	// Stream a bunch of files that are backed by CAS to Artfs.
	AddCasFiles(grpc.ClientStreamingServer[manifest.FileManifest, AddCasFilesReply]) error
	mustEmbedUnimplementedArtfsServer()
}

// UnimplementedArtfsServer must be embedded to have
// forward compatible implementations.
//
// NOTE: this should be embedded by value instead of pointer to avoid a nil
// pointer dereference when methods are called.
type UnimplementedArtfsServer struct{}

func (UnimplementedArtfsServer) AddCasFiles(grpc.ClientStreamingServer[manifest.FileManifest, AddCasFilesReply]) error {
	return status.Errorf(codes.Unimplemented, "method AddCasFiles not implemented")
}
func (UnimplementedArtfsServer) mustEmbedUnimplementedArtfsServer() {}
func (UnimplementedArtfsServer) testEmbeddedByValue()               {}

// UnsafeArtfsServer may be embedded to opt out of forward compatibility for this service.
// Use of this interface is not recommended, as added methods to ArtfsServer will
// result in compilation errors.
type UnsafeArtfsServer interface {
	mustEmbedUnimplementedArtfsServer()
}

func RegisterArtfsServer(s grpc.ServiceRegistrar, srv ArtfsServer) {
	// If the following call pancis, it indicates UnimplementedArtfsServer was
	// embedded by pointer and is nil.  This will cause panics if an
	// unimplemented method is ever invoked, so we test this at initialization
	// time to prevent it from happening at runtime later due to I/O.
	if t, ok := srv.(interface{ testEmbeddedByValue() }); ok {
		t.testEmbeddedByValue()
	}
	s.RegisterService(&Artfs_ServiceDesc, srv)
}

func _Artfs_AddCasFiles_Handler(srv interface{}, stream grpc.ServerStream) error {
	return srv.(ArtfsServer).AddCasFiles(&grpc.GenericServerStream[manifest.FileManifest, AddCasFilesReply]{ServerStream: stream})
}

// This type alias is provided for backwards compatibility with existing code that references the prior non-generic stream type by name.
type Artfs_AddCasFilesServer = grpc.ClientStreamingServer[manifest.FileManifest, AddCasFilesReply]

// Artfs_ServiceDesc is the grpc.ServiceDesc for Artfs service.
// It's only intended for direct use with grpc.RegisterService,
// and not to be introspected or modified (even as a copy)
var Artfs_ServiceDesc = grpc.ServiceDesc{
	ServiceName: "artfs.Artfs",
	HandlerType: (*ArtfsServer)(nil),
	Methods:     []grpc.MethodDesc{},
	Streams: []grpc.StreamDesc{
		{
			StreamName:    "AddCasFiles",
			Handler:       _Artfs_AddCasFiles_Handler,
			ClientStreams: true,
		},
	},
	Metadata: "artfs.proto",
}
