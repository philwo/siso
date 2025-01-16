// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Code generated by protoc-gen-go. DO NOT EDIT.
// versions:
// 	protoc-gen-go v1.36.3
// 	protoc        v5.29.3
// source: rusage.proto

package proto

import (
	protoreflect "google.golang.org/protobuf/reflect/protoreflect"
	protoimpl "google.golang.org/protobuf/runtime/protoimpl"
	durationpb "google.golang.org/protobuf/types/known/durationpb"
	reflect "reflect"
	sync "sync"
)

const (
	// Verify that this generated code is sufficiently up-to-date.
	_ = protoimpl.EnforceVersion(20 - protoimpl.MinVersion)
	// Verify that runtime/protoimpl is sufficiently up-to-date.
	_ = protoimpl.EnforceVersion(protoimpl.MaxVersion - 20)
)

// resource usage of command execution to be stored
// in ActionResult.execution_metadata.auxiliary_metatada.
// https://github.com/bazelbuild/remote-apis/blob/6c32c3b917cc5d3cfee680c03179d7552832bb3f/build/bazel/remote/execution/v2/remote_execution.proto#L1016
type Rusage struct {
	state         protoimpl.MessageState `protogen:"open.v1"`
	MaxRss        int64                  `protobuf:"varint,1,opt,name=max_rss,json=maxRss,proto3" json:"max_rss,omitempty"`
	Majflt        int64                  `protobuf:"varint,2,opt,name=majflt,proto3" json:"majflt,omitempty"`
	Inblock       int64                  `protobuf:"varint,3,opt,name=inblock,proto3" json:"inblock,omitempty"`
	Oublock       int64                  `protobuf:"varint,4,opt,name=oublock,proto3" json:"oublock,omitempty"`
	Utime         *durationpb.Duration   `protobuf:"bytes,5,opt,name=utime,proto3" json:"utime,omitempty"`
	Stime         *durationpb.Duration   `protobuf:"bytes,6,opt,name=stime,proto3" json:"stime,omitempty"`
	unknownFields protoimpl.UnknownFields
	sizeCache     protoimpl.SizeCache
}

func (x *Rusage) Reset() {
	*x = Rusage{}
	mi := &file_rusage_proto_msgTypes[0]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *Rusage) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*Rusage) ProtoMessage() {}

func (x *Rusage) ProtoReflect() protoreflect.Message {
	mi := &file_rusage_proto_msgTypes[0]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use Rusage.ProtoReflect.Descriptor instead.
func (*Rusage) Descriptor() ([]byte, []int) {
	return file_rusage_proto_rawDescGZIP(), []int{0}
}

func (x *Rusage) GetMaxRss() int64 {
	if x != nil {
		return x.MaxRss
	}
	return 0
}

func (x *Rusage) GetMajflt() int64 {
	if x != nil {
		return x.Majflt
	}
	return 0
}

func (x *Rusage) GetInblock() int64 {
	if x != nil {
		return x.Inblock
	}
	return 0
}

func (x *Rusage) GetOublock() int64 {
	if x != nil {
		return x.Oublock
	}
	return 0
}

func (x *Rusage) GetUtime() *durationpb.Duration {
	if x != nil {
		return x.Utime
	}
	return nil
}

func (x *Rusage) GetStime() *durationpb.Duration {
	if x != nil {
		return x.Stime
	}
	return nil
}

var File_rusage_proto protoreflect.FileDescriptor

var file_rusage_proto_rawDesc = []byte{
	0x0a, 0x0c, 0x72, 0x75, 0x73, 0x61, 0x67, 0x65, 0x2e, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x12, 0x0c,
	0x73, 0x69, 0x73, 0x6f, 0x2e, 0x65, 0x78, 0x65, 0x63, 0x75, 0x74, 0x65, 0x1a, 0x1e, 0x67, 0x6f,
	0x6f, 0x67, 0x6c, 0x65, 0x2f, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x62, 0x75, 0x66, 0x2f, 0x64, 0x75,
	0x72, 0x61, 0x74, 0x69, 0x6f, 0x6e, 0x2e, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x22, 0xcf, 0x01, 0x0a,
	0x06, 0x52, 0x75, 0x73, 0x61, 0x67, 0x65, 0x12, 0x17, 0x0a, 0x07, 0x6d, 0x61, 0x78, 0x5f, 0x72,
	0x73, 0x73, 0x18, 0x01, 0x20, 0x01, 0x28, 0x03, 0x52, 0x06, 0x6d, 0x61, 0x78, 0x52, 0x73, 0x73,
	0x12, 0x16, 0x0a, 0x06, 0x6d, 0x61, 0x6a, 0x66, 0x6c, 0x74, 0x18, 0x02, 0x20, 0x01, 0x28, 0x03,
	0x52, 0x06, 0x6d, 0x61, 0x6a, 0x66, 0x6c, 0x74, 0x12, 0x18, 0x0a, 0x07, 0x69, 0x6e, 0x62, 0x6c,
	0x6f, 0x63, 0x6b, 0x18, 0x03, 0x20, 0x01, 0x28, 0x03, 0x52, 0x07, 0x69, 0x6e, 0x62, 0x6c, 0x6f,
	0x63, 0x6b, 0x12, 0x18, 0x0a, 0x07, 0x6f, 0x75, 0x62, 0x6c, 0x6f, 0x63, 0x6b, 0x18, 0x04, 0x20,
	0x01, 0x28, 0x03, 0x52, 0x07, 0x6f, 0x75, 0x62, 0x6c, 0x6f, 0x63, 0x6b, 0x12, 0x2f, 0x0a, 0x05,
	0x75, 0x74, 0x69, 0x6d, 0x65, 0x18, 0x05, 0x20, 0x01, 0x28, 0x0b, 0x32, 0x19, 0x2e, 0x67, 0x6f,
	0x6f, 0x67, 0x6c, 0x65, 0x2e, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x62, 0x75, 0x66, 0x2e, 0x44, 0x75,
	0x72, 0x61, 0x74, 0x69, 0x6f, 0x6e, 0x52, 0x05, 0x75, 0x74, 0x69, 0x6d, 0x65, 0x12, 0x2f, 0x0a,
	0x05, 0x73, 0x74, 0x69, 0x6d, 0x65, 0x18, 0x06, 0x20, 0x01, 0x28, 0x0b, 0x32, 0x19, 0x2e, 0x67,
	0x6f, 0x6f, 0x67, 0x6c, 0x65, 0x2e, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x62, 0x75, 0x66, 0x2e, 0x44,
	0x75, 0x72, 0x61, 0x74, 0x69, 0x6f, 0x6e, 0x52, 0x05, 0x73, 0x74, 0x69, 0x6d, 0x65, 0x42, 0x20,
	0x5a, 0x1e, 0x69, 0x6e, 0x66, 0x72, 0x61, 0x2f, 0x62, 0x75, 0x69, 0x6c, 0x64, 0x2f, 0x73, 0x69,
	0x73, 0x6f, 0x2f, 0x65, 0x78, 0x65, 0x63, 0x75, 0x74, 0x65, 0x2f, 0x70, 0x72, 0x6f, 0x74, 0x6f,
	0x62, 0x06, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x33,
}

var (
	file_rusage_proto_rawDescOnce sync.Once
	file_rusage_proto_rawDescData = file_rusage_proto_rawDesc
)

func file_rusage_proto_rawDescGZIP() []byte {
	file_rusage_proto_rawDescOnce.Do(func() {
		file_rusage_proto_rawDescData = protoimpl.X.CompressGZIP(file_rusage_proto_rawDescData)
	})
	return file_rusage_proto_rawDescData
}

var file_rusage_proto_msgTypes = make([]protoimpl.MessageInfo, 1)
var file_rusage_proto_goTypes = []any{
	(*Rusage)(nil),              // 0: siso.execute.Rusage
	(*durationpb.Duration)(nil), // 1: google.protobuf.Duration
}
var file_rusage_proto_depIdxs = []int32{
	1, // 0: siso.execute.Rusage.utime:type_name -> google.protobuf.Duration
	1, // 1: siso.execute.Rusage.stime:type_name -> google.protobuf.Duration
	2, // [2:2] is the sub-list for method output_type
	2, // [2:2] is the sub-list for method input_type
	2, // [2:2] is the sub-list for extension type_name
	2, // [2:2] is the sub-list for extension extendee
	0, // [0:2] is the sub-list for field type_name
}

func init() { file_rusage_proto_init() }
func file_rusage_proto_init() {
	if File_rusage_proto != nil {
		return
	}
	type x struct{}
	out := protoimpl.TypeBuilder{
		File: protoimpl.DescBuilder{
			GoPackagePath: reflect.TypeOf(x{}).PkgPath(),
			RawDescriptor: file_rusage_proto_rawDesc,
			NumEnums:      0,
			NumMessages:   1,
			NumExtensions: 0,
			NumServices:   0,
		},
		GoTypes:           file_rusage_proto_goTypes,
		DependencyIndexes: file_rusage_proto_depIdxs,
		MessageInfos:      file_rusage_proto_msgTypes,
	}.Build()
	File_rusage_proto = out.File
	file_rusage_proto_rawDesc = nil
	file_rusage_proto_goTypes = nil
	file_rusage_proto_depIdxs = nil
}
