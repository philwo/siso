// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// proto matches with http://shortn/_50RcABcSN9

// Code generated by protoc-gen-go. DO NOT EDIT.
// versions:
// 	protoc-gen-go v1.36.5
// 	protoc        v6.30.2
// source: rbe_auxiliary_metadata.proto

package proto

import (
	protoreflect "google.golang.org/protobuf/reflect/protoreflect"
	protoimpl "google.golang.org/protobuf/runtime/protoimpl"
	reflect "reflect"
	sync "sync"
	unsafe "unsafe"
)

const (
	// Verify that this generated code is sufficiently up-to-date.
	_ = protoimpl.EnforceVersion(20 - protoimpl.MinVersion)
	// Verify that runtime/protoimpl is sufficiently up-to-date.
	_ = protoimpl.EnforceVersion(protoimpl.MaxVersion - 20)
)

type Versions struct {
	state           protoimpl.MessageState `protogen:"open.v1"`
	VmImage         string                 `protobuf:"bytes,1,opt,name=vm_image,json=vmImage,proto3" json:"vm_image,omitempty"`
	BotCodeVersion  string                 `protobuf:"bytes,2,opt,name=bot_code_version,json=botCodeVersion,proto3" json:"bot_code_version,omitempty"`
	DockerRootImage string                 `protobuf:"bytes,3,opt,name=docker_root_image,json=dockerRootImage,proto3" json:"docker_root_image,omitempty"`
	unknownFields   protoimpl.UnknownFields
	sizeCache       protoimpl.SizeCache
}

func (x *Versions) Reset() {
	*x = Versions{}
	mi := &file_rbe_auxiliary_metadata_proto_msgTypes[0]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *Versions) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*Versions) ProtoMessage() {}

func (x *Versions) ProtoReflect() protoreflect.Message {
	mi := &file_rbe_auxiliary_metadata_proto_msgTypes[0]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use Versions.ProtoReflect.Descriptor instead.
func (*Versions) Descriptor() ([]byte, []int) {
	return file_rbe_auxiliary_metadata_proto_rawDescGZIP(), []int{0}
}

func (x *Versions) GetVmImage() string {
	if x != nil {
		return x.VmImage
	}
	return ""
}

func (x *Versions) GetBotCodeVersion() string {
	if x != nil {
		return x.BotCodeVersion
	}
	return ""
}

func (x *Versions) GetDockerRootImage() string {
	if x != nil {
		return x.DockerRootImage
	}
	return ""
}

type ResourceUsage struct {
	state                   protoimpl.MessageState `protogen:"open.v1"`
	CpuPercentagePeak       float64                `protobuf:"fixed64,1,opt,name=cpu_percentage_peak,json=cpuPercentagePeak,proto3" json:"cpu_percentage_peak,omitempty"`
	CpuPercentageAverage    float64                `protobuf:"fixed64,2,opt,name=cpu_percentage_average,json=cpuPercentageAverage,proto3" json:"cpu_percentage_average,omitempty"`
	MemoryPercentagePeak    float64                `protobuf:"fixed64,3,opt,name=memory_percentage_peak,json=memoryPercentagePeak,proto3" json:"memory_percentage_peak,omitempty"`
	MemoryPercentageAverage float64                `protobuf:"fixed64,4,opt,name=memory_percentage_average,json=memoryPercentageAverage,proto3" json:"memory_percentage_average,omitempty"`
	unknownFields           protoimpl.UnknownFields
	sizeCache               protoimpl.SizeCache
}

func (x *ResourceUsage) Reset() {
	*x = ResourceUsage{}
	mi := &file_rbe_auxiliary_metadata_proto_msgTypes[1]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *ResourceUsage) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*ResourceUsage) ProtoMessage() {}

func (x *ResourceUsage) ProtoReflect() protoreflect.Message {
	mi := &file_rbe_auxiliary_metadata_proto_msgTypes[1]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use ResourceUsage.ProtoReflect.Descriptor instead.
func (*ResourceUsage) Descriptor() ([]byte, []int) {
	return file_rbe_auxiliary_metadata_proto_rawDescGZIP(), []int{1}
}

func (x *ResourceUsage) GetCpuPercentagePeak() float64 {
	if x != nil {
		return x.CpuPercentagePeak
	}
	return 0
}

func (x *ResourceUsage) GetCpuPercentageAverage() float64 {
	if x != nil {
		return x.CpuPercentageAverage
	}
	return 0
}

func (x *ResourceUsage) GetMemoryPercentagePeak() float64 {
	if x != nil {
		return x.MemoryPercentagePeak
	}
	return 0
}

func (x *ResourceUsage) GetMemoryPercentageAverage() float64 {
	if x != nil {
		return x.MemoryPercentageAverage
	}
	return 0
}

type AuxiliaryMetadata struct {
	state         protoimpl.MessageState `protogen:"open.v1"`
	Versions      *Versions              `protobuf:"bytes,1,opt,name=versions,proto3" json:"versions,omitempty"`
	Pool          string                 `protobuf:"bytes,2,opt,name=pool,proto3" json:"pool,omitempty"`
	Usage         *ResourceUsage         `protobuf:"bytes,3,opt,name=usage,proto3" json:"usage,omitempty"`
	unknownFields protoimpl.UnknownFields
	sizeCache     protoimpl.SizeCache
}

func (x *AuxiliaryMetadata) Reset() {
	*x = AuxiliaryMetadata{}
	mi := &file_rbe_auxiliary_metadata_proto_msgTypes[2]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *AuxiliaryMetadata) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*AuxiliaryMetadata) ProtoMessage() {}

func (x *AuxiliaryMetadata) ProtoReflect() protoreflect.Message {
	mi := &file_rbe_auxiliary_metadata_proto_msgTypes[2]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use AuxiliaryMetadata.ProtoReflect.Descriptor instead.
func (*AuxiliaryMetadata) Descriptor() ([]byte, []int) {
	return file_rbe_auxiliary_metadata_proto_rawDescGZIP(), []int{2}
}

func (x *AuxiliaryMetadata) GetVersions() *Versions {
	if x != nil {
		return x.Versions
	}
	return nil
}

func (x *AuxiliaryMetadata) GetPool() string {
	if x != nil {
		return x.Pool
	}
	return ""
}

func (x *AuxiliaryMetadata) GetUsage() *ResourceUsage {
	if x != nil {
		return x.Usage
	}
	return nil
}

var File_rbe_auxiliary_metadata_proto protoreflect.FileDescriptor

var file_rbe_auxiliary_metadata_proto_rawDesc = string([]byte{
	0x0a, 0x1c, 0x72, 0x62, 0x65, 0x5f, 0x61, 0x75, 0x78, 0x69, 0x6c, 0x69, 0x61, 0x72, 0x79, 0x5f,
	0x6d, 0x65, 0x74, 0x61, 0x64, 0x61, 0x74, 0x61, 0x2e, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x12, 0x30,
	0x64, 0x65, 0x76, 0x74, 0x6f, 0x6f, 0x6c, 0x73, 0x5f, 0x66, 0x6f, 0x75, 0x6e, 0x64, 0x72, 0x79,
	0x5f, 0x61, 0x70, 0x69, 0x5f, 0x72, 0x65, 0x6d, 0x6f, 0x74, 0x65, 0x5f, 0x65, 0x78, 0x65, 0x63,
	0x75, 0x74, 0x69, 0x6f, 0x6e, 0x5f, 0x65, 0x78, 0x74, 0x65, 0x6e, 0x73, 0x69, 0x6f, 0x6e, 0x73,
	0x22, 0x7b, 0x0a, 0x08, 0x56, 0x65, 0x72, 0x73, 0x69, 0x6f, 0x6e, 0x73, 0x12, 0x19, 0x0a, 0x08,
	0x76, 0x6d, 0x5f, 0x69, 0x6d, 0x61, 0x67, 0x65, 0x18, 0x01, 0x20, 0x01, 0x28, 0x09, 0x52, 0x07,
	0x76, 0x6d, 0x49, 0x6d, 0x61, 0x67, 0x65, 0x12, 0x28, 0x0a, 0x10, 0x62, 0x6f, 0x74, 0x5f, 0x63,
	0x6f, 0x64, 0x65, 0x5f, 0x76, 0x65, 0x72, 0x73, 0x69, 0x6f, 0x6e, 0x18, 0x02, 0x20, 0x01, 0x28,
	0x09, 0x52, 0x0e, 0x62, 0x6f, 0x74, 0x43, 0x6f, 0x64, 0x65, 0x56, 0x65, 0x72, 0x73, 0x69, 0x6f,
	0x6e, 0x12, 0x2a, 0x0a, 0x11, 0x64, 0x6f, 0x63, 0x6b, 0x65, 0x72, 0x5f, 0x72, 0x6f, 0x6f, 0x74,
	0x5f, 0x69, 0x6d, 0x61, 0x67, 0x65, 0x18, 0x03, 0x20, 0x01, 0x28, 0x09, 0x52, 0x0f, 0x64, 0x6f,
	0x63, 0x6b, 0x65, 0x72, 0x52, 0x6f, 0x6f, 0x74, 0x49, 0x6d, 0x61, 0x67, 0x65, 0x22, 0xe7, 0x01,
	0x0a, 0x0d, 0x52, 0x65, 0x73, 0x6f, 0x75, 0x72, 0x63, 0x65, 0x55, 0x73, 0x61, 0x67, 0x65, 0x12,
	0x2e, 0x0a, 0x13, 0x63, 0x70, 0x75, 0x5f, 0x70, 0x65, 0x72, 0x63, 0x65, 0x6e, 0x74, 0x61, 0x67,
	0x65, 0x5f, 0x70, 0x65, 0x61, 0x6b, 0x18, 0x01, 0x20, 0x01, 0x28, 0x01, 0x52, 0x11, 0x63, 0x70,
	0x75, 0x50, 0x65, 0x72, 0x63, 0x65, 0x6e, 0x74, 0x61, 0x67, 0x65, 0x50, 0x65, 0x61, 0x6b, 0x12,
	0x34, 0x0a, 0x16, 0x63, 0x70, 0x75, 0x5f, 0x70, 0x65, 0x72, 0x63, 0x65, 0x6e, 0x74, 0x61, 0x67,
	0x65, 0x5f, 0x61, 0x76, 0x65, 0x72, 0x61, 0x67, 0x65, 0x18, 0x02, 0x20, 0x01, 0x28, 0x01, 0x52,
	0x14, 0x63, 0x70, 0x75, 0x50, 0x65, 0x72, 0x63, 0x65, 0x6e, 0x74, 0x61, 0x67, 0x65, 0x41, 0x76,
	0x65, 0x72, 0x61, 0x67, 0x65, 0x12, 0x34, 0x0a, 0x16, 0x6d, 0x65, 0x6d, 0x6f, 0x72, 0x79, 0x5f,
	0x70, 0x65, 0x72, 0x63, 0x65, 0x6e, 0x74, 0x61, 0x67, 0x65, 0x5f, 0x70, 0x65, 0x61, 0x6b, 0x18,
	0x03, 0x20, 0x01, 0x28, 0x01, 0x52, 0x14, 0x6d, 0x65, 0x6d, 0x6f, 0x72, 0x79, 0x50, 0x65, 0x72,
	0x63, 0x65, 0x6e, 0x74, 0x61, 0x67, 0x65, 0x50, 0x65, 0x61, 0x6b, 0x12, 0x3a, 0x0a, 0x19, 0x6d,
	0x65, 0x6d, 0x6f, 0x72, 0x79, 0x5f, 0x70, 0x65, 0x72, 0x63, 0x65, 0x6e, 0x74, 0x61, 0x67, 0x65,
	0x5f, 0x61, 0x76, 0x65, 0x72, 0x61, 0x67, 0x65, 0x18, 0x04, 0x20, 0x01, 0x28, 0x01, 0x52, 0x17,
	0x6d, 0x65, 0x6d, 0x6f, 0x72, 0x79, 0x50, 0x65, 0x72, 0x63, 0x65, 0x6e, 0x74, 0x61, 0x67, 0x65,
	0x41, 0x76, 0x65, 0x72, 0x61, 0x67, 0x65, 0x22, 0xd6, 0x01, 0x0a, 0x11, 0x41, 0x75, 0x78, 0x69,
	0x6c, 0x69, 0x61, 0x72, 0x79, 0x4d, 0x65, 0x74, 0x61, 0x64, 0x61, 0x74, 0x61, 0x12, 0x56, 0x0a,
	0x08, 0x76, 0x65, 0x72, 0x73, 0x69, 0x6f, 0x6e, 0x73, 0x18, 0x01, 0x20, 0x01, 0x28, 0x0b, 0x32,
	0x3a, 0x2e, 0x64, 0x65, 0x76, 0x74, 0x6f, 0x6f, 0x6c, 0x73, 0x5f, 0x66, 0x6f, 0x75, 0x6e, 0x64,
	0x72, 0x79, 0x5f, 0x61, 0x70, 0x69, 0x5f, 0x72, 0x65, 0x6d, 0x6f, 0x74, 0x65, 0x5f, 0x65, 0x78,
	0x65, 0x63, 0x75, 0x74, 0x69, 0x6f, 0x6e, 0x5f, 0x65, 0x78, 0x74, 0x65, 0x6e, 0x73, 0x69, 0x6f,
	0x6e, 0x73, 0x2e, 0x56, 0x65, 0x72, 0x73, 0x69, 0x6f, 0x6e, 0x73, 0x52, 0x08, 0x76, 0x65, 0x72,
	0x73, 0x69, 0x6f, 0x6e, 0x73, 0x12, 0x12, 0x0a, 0x04, 0x70, 0x6f, 0x6f, 0x6c, 0x18, 0x02, 0x20,
	0x01, 0x28, 0x09, 0x52, 0x04, 0x70, 0x6f, 0x6f, 0x6c, 0x12, 0x55, 0x0a, 0x05, 0x75, 0x73, 0x61,
	0x67, 0x65, 0x18, 0x03, 0x20, 0x01, 0x28, 0x0b, 0x32, 0x3f, 0x2e, 0x64, 0x65, 0x76, 0x74, 0x6f,
	0x6f, 0x6c, 0x73, 0x5f, 0x66, 0x6f, 0x75, 0x6e, 0x64, 0x72, 0x79, 0x5f, 0x61, 0x70, 0x69, 0x5f,
	0x72, 0x65, 0x6d, 0x6f, 0x74, 0x65, 0x5f, 0x65, 0x78, 0x65, 0x63, 0x75, 0x74, 0x69, 0x6f, 0x6e,
	0x5f, 0x65, 0x78, 0x74, 0x65, 0x6e, 0x73, 0x69, 0x6f, 0x6e, 0x73, 0x2e, 0x52, 0x65, 0x73, 0x6f,
	0x75, 0x72, 0x63, 0x65, 0x55, 0x73, 0x61, 0x67, 0x65, 0x52, 0x05, 0x75, 0x73, 0x61, 0x67, 0x65,
	0x42, 0x2e, 0x5a, 0x2c, 0x67, 0x6f, 0x2e, 0x63, 0x68, 0x72, 0x6f, 0x6d, 0x69, 0x75, 0x6d, 0x2e,
	0x6f, 0x72, 0x67, 0x2f, 0x69, 0x6e, 0x66, 0x72, 0x61, 0x2f, 0x62, 0x75, 0x69, 0x6c, 0x64, 0x2f,
	0x73, 0x69, 0x73, 0x6f, 0x2f, 0x72, 0x65, 0x61, 0x70, 0x69, 0x2f, 0x70, 0x72, 0x6f, 0x74, 0x6f,
	0x62, 0x06, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x33,
})

var (
	file_rbe_auxiliary_metadata_proto_rawDescOnce sync.Once
	file_rbe_auxiliary_metadata_proto_rawDescData []byte
)

func file_rbe_auxiliary_metadata_proto_rawDescGZIP() []byte {
	file_rbe_auxiliary_metadata_proto_rawDescOnce.Do(func() {
		file_rbe_auxiliary_metadata_proto_rawDescData = protoimpl.X.CompressGZIP(unsafe.Slice(unsafe.StringData(file_rbe_auxiliary_metadata_proto_rawDesc), len(file_rbe_auxiliary_metadata_proto_rawDesc)))
	})
	return file_rbe_auxiliary_metadata_proto_rawDescData
}

var file_rbe_auxiliary_metadata_proto_msgTypes = make([]protoimpl.MessageInfo, 3)
var file_rbe_auxiliary_metadata_proto_goTypes = []any{
	(*Versions)(nil),          // 0: devtools_foundry_api_remote_execution_extensions.Versions
	(*ResourceUsage)(nil),     // 1: devtools_foundry_api_remote_execution_extensions.ResourceUsage
	(*AuxiliaryMetadata)(nil), // 2: devtools_foundry_api_remote_execution_extensions.AuxiliaryMetadata
}
var file_rbe_auxiliary_metadata_proto_depIdxs = []int32{
	0, // 0: devtools_foundry_api_remote_execution_extensions.AuxiliaryMetadata.versions:type_name -> devtools_foundry_api_remote_execution_extensions.Versions
	1, // 1: devtools_foundry_api_remote_execution_extensions.AuxiliaryMetadata.usage:type_name -> devtools_foundry_api_remote_execution_extensions.ResourceUsage
	2, // [2:2] is the sub-list for method output_type
	2, // [2:2] is the sub-list for method input_type
	2, // [2:2] is the sub-list for extension type_name
	2, // [2:2] is the sub-list for extension extendee
	0, // [0:2] is the sub-list for field type_name
}

func init() { file_rbe_auxiliary_metadata_proto_init() }
func file_rbe_auxiliary_metadata_proto_init() {
	if File_rbe_auxiliary_metadata_proto != nil {
		return
	}
	type x struct{}
	out := protoimpl.TypeBuilder{
		File: protoimpl.DescBuilder{
			GoPackagePath: reflect.TypeOf(x{}).PkgPath(),
			RawDescriptor: unsafe.Slice(unsafe.StringData(file_rbe_auxiliary_metadata_proto_rawDesc), len(file_rbe_auxiliary_metadata_proto_rawDesc)),
			NumEnums:      0,
			NumMessages:   3,
			NumExtensions: 0,
			NumServices:   0,
		},
		GoTypes:           file_rbe_auxiliary_metadata_proto_goTypes,
		DependencyIndexes: file_rbe_auxiliary_metadata_proto_depIdxs,
		MessageInfos:      file_rbe_auxiliary_metadata_proto_msgTypes,
	}.Build()
	File_rbe_auxiliary_metadata_proto = out.File
	file_rbe_auxiliary_metadata_proto_goTypes = nil
	file_rbe_auxiliary_metadata_proto_depIdxs = nil
}
