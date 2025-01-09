// Code generated by protoc-gen-go. DO NOT EDIT.
// versions:
// 	protoc-gen-go v1.36.2
// 	protoc        v5.29.3
// source: manifest.proto

package manifest

import (
	protoreflect "google.golang.org/protobuf/reflect/protoreflect"
	protoimpl "google.golang.org/protobuf/runtime/protoimpl"
	timestamppb "google.golang.org/protobuf/types/known/timestamppb"
	wrapperspb "google.golang.org/protobuf/types/known/wrapperspb"
	reflect "reflect"
	sync "sync"
)

const (
	// Verify that this generated code is sufficiently up-to-date.
	_ = protoimpl.EnforceVersion(20 - protoimpl.MinVersion)
	// Verify that runtime/protoimpl is sufficiently up-to-date.
	_ = protoimpl.EnforceVersion(protoimpl.MaxVersion - 20)
)

type Manifest struct {
	state protoimpl.MessageState `protogen:"open.v1"`
	// Map of all files from filepath to FileManifest.
	Files         map[string]*FileManifest `protobuf:"bytes,1,rep,name=files,proto3" json:"files,omitempty" protobuf_key:"bytes,1,opt,name=key" protobuf_val:"bytes,2,opt,name=value"`
	unknownFields protoimpl.UnknownFields
	sizeCache     protoimpl.SizeCache
}

func (x *Manifest) Reset() {
	*x = Manifest{}
	mi := &file_manifest_proto_msgTypes[0]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *Manifest) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*Manifest) ProtoMessage() {}

func (x *Manifest) ProtoReflect() protoreflect.Message {
	mi := &file_manifest_proto_msgTypes[0]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use Manifest.ProtoReflect.Descriptor instead.
func (*Manifest) Descriptor() ([]byte, []int) {
	return file_manifest_proto_rawDescGZIP(), []int{0}
}

func (x *Manifest) GetFiles() map[string]*FileManifest {
	if x != nil {
		return x.Files
	}
	return nil
}

// This proto is intended to match with the tree.TreeOutput struct in the
// remote-apis-sdks, so that we can reuse some of the functions for downloading.
type FileManifest struct {
	state protoimpl.MessageState `protogen:"open.v1"`
	// The digest of the file's content.
	Digest *Digest `protobuf:"bytes,1,opt,name=digest,proto3" json:"digest,omitempty"`
	// The path of the file.
	Path string `protobuf:"bytes,2,opt,name=path,proto3" json:"path,omitempty"`
	// True if this file is executable, false otherwise.
	IsExecutable bool `protobuf:"varint,3,opt,name=is_executable,json=isExecutable,proto3" json:"is_executable,omitempty"`
	// True if path is an empty directory.
	IsEmptyDirectory bool `protobuf:"varint,4,opt,name=is_empty_directory,json=isEmptyDirectory,proto3" json:"is_empty_directory,omitempty"`
	// If it is a symlink, then its target path.
	SymlinkTarget string `protobuf:"bytes,5,opt,name=symlink_target,json=symlinkTarget,proto3" json:"symlink_target,omitempty"`
	// Properties for each FileManifest entry.
	NodeProperties *NodeProperties `protobuf:"bytes,6,opt,name=node_properties,json=nodeProperties,proto3" json:"node_properties,omitempty"`
	unknownFields  protoimpl.UnknownFields
	sizeCache      protoimpl.SizeCache
}

func (x *FileManifest) Reset() {
	*x = FileManifest{}
	mi := &file_manifest_proto_msgTypes[1]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *FileManifest) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*FileManifest) ProtoMessage() {}

func (x *FileManifest) ProtoReflect() protoreflect.Message {
	mi := &file_manifest_proto_msgTypes[1]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use FileManifest.ProtoReflect.Descriptor instead.
func (*FileManifest) Descriptor() ([]byte, []int) {
	return file_manifest_proto_rawDescGZIP(), []int{1}
}

func (x *FileManifest) GetDigest() *Digest {
	if x != nil {
		return x.Digest
	}
	return nil
}

func (x *FileManifest) GetPath() string {
	if x != nil {
		return x.Path
	}
	return ""
}

func (x *FileManifest) GetIsExecutable() bool {
	if x != nil {
		return x.IsExecutable
	}
	return false
}

func (x *FileManifest) GetIsEmptyDirectory() bool {
	if x != nil {
		return x.IsEmptyDirectory
	}
	return false
}

func (x *FileManifest) GetSymlinkTarget() string {
	if x != nil {
		return x.SymlinkTarget
	}
	return ""
}

func (x *FileManifest) GetNodeProperties() *NodeProperties {
	if x != nil {
		return x.NodeProperties
	}
	return nil
}

// A copy of NodeProperties from https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto
// to avoid importing it as a proto dependency.
type NodeProperties struct {
	state protoimpl.MessageState `protogen:"open.v1"`
	// A list of string-based
	// [NodeProperties][build.bazel.remote.execution.v2.NodeProperty].
	Properties []*NodeProperty `protobuf:"bytes,1,rep,name=properties,proto3" json:"properties,omitempty"`
	// The file's last modification timestamp.
	Mtime *timestamppb.Timestamp `protobuf:"bytes,2,opt,name=mtime,proto3" json:"mtime,omitempty"`
	// The UNIX file mode, e.g., 0755.
	UnixMode      *wrapperspb.UInt32Value `protobuf:"bytes,3,opt,name=unix_mode,json=unixMode,proto3" json:"unix_mode,omitempty"`
	unknownFields protoimpl.UnknownFields
	sizeCache     protoimpl.SizeCache
}

func (x *NodeProperties) Reset() {
	*x = NodeProperties{}
	mi := &file_manifest_proto_msgTypes[2]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *NodeProperties) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*NodeProperties) ProtoMessage() {}

func (x *NodeProperties) ProtoReflect() protoreflect.Message {
	mi := &file_manifest_proto_msgTypes[2]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use NodeProperties.ProtoReflect.Descriptor instead.
func (*NodeProperties) Descriptor() ([]byte, []int) {
	return file_manifest_proto_rawDescGZIP(), []int{2}
}

func (x *NodeProperties) GetProperties() []*NodeProperty {
	if x != nil {
		return x.Properties
	}
	return nil
}

func (x *NodeProperties) GetMtime() *timestamppb.Timestamp {
	if x != nil {
		return x.Mtime
	}
	return nil
}

func (x *NodeProperties) GetUnixMode() *wrapperspb.UInt32Value {
	if x != nil {
		return x.UnixMode
	}
	return nil
}

// A copy of NodeProperty from https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto
// to avoid importing it as a proto dependency.
type NodeProperty struct {
	state protoimpl.MessageState `protogen:"open.v1"`
	// The property name.
	Name string `protobuf:"bytes,1,opt,name=name,proto3" json:"name,omitempty"`
	// The property value.
	Value         string `protobuf:"bytes,2,opt,name=value,proto3" json:"value,omitempty"`
	unknownFields protoimpl.UnknownFields
	sizeCache     protoimpl.SizeCache
}

func (x *NodeProperty) Reset() {
	*x = NodeProperty{}
	mi := &file_manifest_proto_msgTypes[3]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *NodeProperty) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*NodeProperty) ProtoMessage() {}

func (x *NodeProperty) ProtoReflect() protoreflect.Message {
	mi := &file_manifest_proto_msgTypes[3]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use NodeProperty.ProtoReflect.Descriptor instead.
func (*NodeProperty) Descriptor() ([]byte, []int) {
	return file_manifest_proto_rawDescGZIP(), []int{3}
}

func (x *NodeProperty) GetName() string {
	if x != nil {
		return x.Name
	}
	return ""
}

func (x *NodeProperty) GetValue() string {
	if x != nil {
		return x.Value
	}
	return ""
}

// A copy of Digest from https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto
// to avoid importing it as a proto dependency.
type Digest struct {
	state protoimpl.MessageState `protogen:"open.v1"`
	// The hash, represented as a lowercase hexadecimal string, padded with
	// leading zeroes up to the hash function length.
	Hash string `protobuf:"bytes,1,opt,name=hash,proto3" json:"hash,omitempty"`
	// The size of the blob, in bytes.
	SizeBytes     int64 `protobuf:"varint,2,opt,name=size_bytes,json=sizeBytes,proto3" json:"size_bytes,omitempty"`
	unknownFields protoimpl.UnknownFields
	sizeCache     protoimpl.SizeCache
}

func (x *Digest) Reset() {
	*x = Digest{}
	mi := &file_manifest_proto_msgTypes[4]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *Digest) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*Digest) ProtoMessage() {}

func (x *Digest) ProtoReflect() protoreflect.Message {
	mi := &file_manifest_proto_msgTypes[4]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use Digest.ProtoReflect.Descriptor instead.
func (*Digest) Descriptor() ([]byte, []int) {
	return file_manifest_proto_rawDescGZIP(), []int{4}
}

func (x *Digest) GetHash() string {
	if x != nil {
		return x.Hash
	}
	return ""
}

func (x *Digest) GetSizeBytes() int64 {
	if x != nil {
		return x.SizeBytes
	}
	return 0
}

var File_manifest_proto protoreflect.FileDescriptor

var file_manifest_proto_rawDesc = []byte{
	0x0a, 0x0e, 0x6d, 0x61, 0x6e, 0x69, 0x66, 0x65, 0x73, 0x74, 0x2e, 0x70, 0x72, 0x6f, 0x74, 0x6f,
	0x12, 0x08, 0x6d, 0x61, 0x6e, 0x69, 0x66, 0x65, 0x73, 0x74, 0x1a, 0x1f, 0x67, 0x6f, 0x6f, 0x67,
	0x6c, 0x65, 0x2f, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x62, 0x75, 0x66, 0x2f, 0x74, 0x69, 0x6d, 0x65,
	0x73, 0x74, 0x61, 0x6d, 0x70, 0x2e, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x1a, 0x1e, 0x67, 0x6f, 0x6f,
	0x67, 0x6c, 0x65, 0x2f, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x62, 0x75, 0x66, 0x2f, 0x77, 0x72, 0x61,
	0x70, 0x70, 0x65, 0x72, 0x73, 0x2e, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x22, 0x91, 0x01, 0x0a, 0x08,
	0x4d, 0x61, 0x6e, 0x69, 0x66, 0x65, 0x73, 0x74, 0x12, 0x33, 0x0a, 0x05, 0x66, 0x69, 0x6c, 0x65,
	0x73, 0x18, 0x01, 0x20, 0x03, 0x28, 0x0b, 0x32, 0x1d, 0x2e, 0x6d, 0x61, 0x6e, 0x69, 0x66, 0x65,
	0x73, 0x74, 0x2e, 0x4d, 0x61, 0x6e, 0x69, 0x66, 0x65, 0x73, 0x74, 0x2e, 0x46, 0x69, 0x6c, 0x65,
	0x73, 0x45, 0x6e, 0x74, 0x72, 0x79, 0x52, 0x05, 0x66, 0x69, 0x6c, 0x65, 0x73, 0x1a, 0x50, 0x0a,
	0x0a, 0x46, 0x69, 0x6c, 0x65, 0x73, 0x45, 0x6e, 0x74, 0x72, 0x79, 0x12, 0x10, 0x0a, 0x03, 0x6b,
	0x65, 0x79, 0x18, 0x01, 0x20, 0x01, 0x28, 0x09, 0x52, 0x03, 0x6b, 0x65, 0x79, 0x12, 0x2c, 0x0a,
	0x05, 0x76, 0x61, 0x6c, 0x75, 0x65, 0x18, 0x02, 0x20, 0x01, 0x28, 0x0b, 0x32, 0x16, 0x2e, 0x6d,
	0x61, 0x6e, 0x69, 0x66, 0x65, 0x73, 0x74, 0x2e, 0x46, 0x69, 0x6c, 0x65, 0x4d, 0x61, 0x6e, 0x69,
	0x66, 0x65, 0x73, 0x74, 0x52, 0x05, 0x76, 0x61, 0x6c, 0x75, 0x65, 0x3a, 0x02, 0x38, 0x01, 0x22,
	0x89, 0x02, 0x0a, 0x0c, 0x46, 0x69, 0x6c, 0x65, 0x4d, 0x61, 0x6e, 0x69, 0x66, 0x65, 0x73, 0x74,
	0x12, 0x28, 0x0a, 0x06, 0x64, 0x69, 0x67, 0x65, 0x73, 0x74, 0x18, 0x01, 0x20, 0x01, 0x28, 0x0b,
	0x32, 0x10, 0x2e, 0x6d, 0x61, 0x6e, 0x69, 0x66, 0x65, 0x73, 0x74, 0x2e, 0x44, 0x69, 0x67, 0x65,
	0x73, 0x74, 0x52, 0x06, 0x64, 0x69, 0x67, 0x65, 0x73, 0x74, 0x12, 0x12, 0x0a, 0x04, 0x70, 0x61,
	0x74, 0x68, 0x18, 0x02, 0x20, 0x01, 0x28, 0x09, 0x52, 0x04, 0x70, 0x61, 0x74, 0x68, 0x12, 0x23,
	0x0a, 0x0d, 0x69, 0x73, 0x5f, 0x65, 0x78, 0x65, 0x63, 0x75, 0x74, 0x61, 0x62, 0x6c, 0x65, 0x18,
	0x03, 0x20, 0x01, 0x28, 0x08, 0x52, 0x0c, 0x69, 0x73, 0x45, 0x78, 0x65, 0x63, 0x75, 0x74, 0x61,
	0x62, 0x6c, 0x65, 0x12, 0x2c, 0x0a, 0x12, 0x69, 0x73, 0x5f, 0x65, 0x6d, 0x70, 0x74, 0x79, 0x5f,
	0x64, 0x69, 0x72, 0x65, 0x63, 0x74, 0x6f, 0x72, 0x79, 0x18, 0x04, 0x20, 0x01, 0x28, 0x08, 0x52,
	0x10, 0x69, 0x73, 0x45, 0x6d, 0x70, 0x74, 0x79, 0x44, 0x69, 0x72, 0x65, 0x63, 0x74, 0x6f, 0x72,
	0x79, 0x12, 0x25, 0x0a, 0x0e, 0x73, 0x79, 0x6d, 0x6c, 0x69, 0x6e, 0x6b, 0x5f, 0x74, 0x61, 0x72,
	0x67, 0x65, 0x74, 0x18, 0x05, 0x20, 0x01, 0x28, 0x09, 0x52, 0x0d, 0x73, 0x79, 0x6d, 0x6c, 0x69,
	0x6e, 0x6b, 0x54, 0x61, 0x72, 0x67, 0x65, 0x74, 0x12, 0x41, 0x0a, 0x0f, 0x6e, 0x6f, 0x64, 0x65,
	0x5f, 0x70, 0x72, 0x6f, 0x70, 0x65, 0x72, 0x74, 0x69, 0x65, 0x73, 0x18, 0x06, 0x20, 0x01, 0x28,
	0x0b, 0x32, 0x18, 0x2e, 0x6d, 0x61, 0x6e, 0x69, 0x66, 0x65, 0x73, 0x74, 0x2e, 0x4e, 0x6f, 0x64,
	0x65, 0x50, 0x72, 0x6f, 0x70, 0x65, 0x72, 0x74, 0x69, 0x65, 0x73, 0x52, 0x0e, 0x6e, 0x6f, 0x64,
	0x65, 0x50, 0x72, 0x6f, 0x70, 0x65, 0x72, 0x74, 0x69, 0x65, 0x73, 0x22, 0xb5, 0x01, 0x0a, 0x0e,
	0x4e, 0x6f, 0x64, 0x65, 0x50, 0x72, 0x6f, 0x70, 0x65, 0x72, 0x74, 0x69, 0x65, 0x73, 0x12, 0x36,
	0x0a, 0x0a, 0x70, 0x72, 0x6f, 0x70, 0x65, 0x72, 0x74, 0x69, 0x65, 0x73, 0x18, 0x01, 0x20, 0x03,
	0x28, 0x0b, 0x32, 0x16, 0x2e, 0x6d, 0x61, 0x6e, 0x69, 0x66, 0x65, 0x73, 0x74, 0x2e, 0x4e, 0x6f,
	0x64, 0x65, 0x50, 0x72, 0x6f, 0x70, 0x65, 0x72, 0x74, 0x79, 0x52, 0x0a, 0x70, 0x72, 0x6f, 0x70,
	0x65, 0x72, 0x74, 0x69, 0x65, 0x73, 0x12, 0x30, 0x0a, 0x05, 0x6d, 0x74, 0x69, 0x6d, 0x65, 0x18,
	0x02, 0x20, 0x01, 0x28, 0x0b, 0x32, 0x1a, 0x2e, 0x67, 0x6f, 0x6f, 0x67, 0x6c, 0x65, 0x2e, 0x70,
	0x72, 0x6f, 0x74, 0x6f, 0x62, 0x75, 0x66, 0x2e, 0x54, 0x69, 0x6d, 0x65, 0x73, 0x74, 0x61, 0x6d,
	0x70, 0x52, 0x05, 0x6d, 0x74, 0x69, 0x6d, 0x65, 0x12, 0x39, 0x0a, 0x09, 0x75, 0x6e, 0x69, 0x78,
	0x5f, 0x6d, 0x6f, 0x64, 0x65, 0x18, 0x03, 0x20, 0x01, 0x28, 0x0b, 0x32, 0x1c, 0x2e, 0x67, 0x6f,
	0x6f, 0x67, 0x6c, 0x65, 0x2e, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x62, 0x75, 0x66, 0x2e, 0x55, 0x49,
	0x6e, 0x74, 0x33, 0x32, 0x56, 0x61, 0x6c, 0x75, 0x65, 0x52, 0x08, 0x75, 0x6e, 0x69, 0x78, 0x4d,
	0x6f, 0x64, 0x65, 0x22, 0x38, 0x0a, 0x0c, 0x4e, 0x6f, 0x64, 0x65, 0x50, 0x72, 0x6f, 0x70, 0x65,
	0x72, 0x74, 0x79, 0x12, 0x12, 0x0a, 0x04, 0x6e, 0x61, 0x6d, 0x65, 0x18, 0x01, 0x20, 0x01, 0x28,
	0x09, 0x52, 0x04, 0x6e, 0x61, 0x6d, 0x65, 0x12, 0x14, 0x0a, 0x05, 0x76, 0x61, 0x6c, 0x75, 0x65,
	0x18, 0x02, 0x20, 0x01, 0x28, 0x09, 0x52, 0x05, 0x76, 0x61, 0x6c, 0x75, 0x65, 0x22, 0x3b, 0x0a,
	0x06, 0x44, 0x69, 0x67, 0x65, 0x73, 0x74, 0x12, 0x12, 0x0a, 0x04, 0x68, 0x61, 0x73, 0x68, 0x18,
	0x01, 0x20, 0x01, 0x28, 0x09, 0x52, 0x04, 0x68, 0x61, 0x73, 0x68, 0x12, 0x1d, 0x0a, 0x0a, 0x73,
	0x69, 0x7a, 0x65, 0x5f, 0x62, 0x79, 0x74, 0x65, 0x73, 0x18, 0x02, 0x20, 0x01, 0x28, 0x03, 0x52,
	0x09, 0x73, 0x69, 0x7a, 0x65, 0x42, 0x79, 0x74, 0x65, 0x73, 0x42, 0x37, 0x5a, 0x35, 0x69, 0x6e,
	0x66, 0x72, 0x61, 0x2f, 0x62, 0x75, 0x69, 0x6c, 0x64, 0x2f, 0x73, 0x69, 0x73, 0x6f, 0x2f, 0x74,
	0x6f, 0x6f, 0x6c, 0x73, 0x75, 0x70, 0x70, 0x6f, 0x72, 0x74, 0x2f, 0x61, 0x72, 0x74, 0x66, 0x73,
	0x75, 0x74, 0x69, 0x6c, 0x2f, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x2f, 0x6d, 0x61, 0x6e, 0x69, 0x66,
	0x65, 0x73, 0x74, 0x62, 0x06, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x33,
}

var (
	file_manifest_proto_rawDescOnce sync.Once
	file_manifest_proto_rawDescData = file_manifest_proto_rawDesc
)

func file_manifest_proto_rawDescGZIP() []byte {
	file_manifest_proto_rawDescOnce.Do(func() {
		file_manifest_proto_rawDescData = protoimpl.X.CompressGZIP(file_manifest_proto_rawDescData)
	})
	return file_manifest_proto_rawDescData
}

var file_manifest_proto_msgTypes = make([]protoimpl.MessageInfo, 6)
var file_manifest_proto_goTypes = []any{
	(*Manifest)(nil),               // 0: manifest.Manifest
	(*FileManifest)(nil),           // 1: manifest.FileManifest
	(*NodeProperties)(nil),         // 2: manifest.NodeProperties
	(*NodeProperty)(nil),           // 3: manifest.NodeProperty
	(*Digest)(nil),                 // 4: manifest.Digest
	nil,                            // 5: manifest.Manifest.FilesEntry
	(*timestamppb.Timestamp)(nil),  // 6: google.protobuf.Timestamp
	(*wrapperspb.UInt32Value)(nil), // 7: google.protobuf.UInt32Value
}
var file_manifest_proto_depIdxs = []int32{
	5, // 0: manifest.Manifest.files:type_name -> manifest.Manifest.FilesEntry
	4, // 1: manifest.FileManifest.digest:type_name -> manifest.Digest
	2, // 2: manifest.FileManifest.node_properties:type_name -> manifest.NodeProperties
	3, // 3: manifest.NodeProperties.properties:type_name -> manifest.NodeProperty
	6, // 4: manifest.NodeProperties.mtime:type_name -> google.protobuf.Timestamp
	7, // 5: manifest.NodeProperties.unix_mode:type_name -> google.protobuf.UInt32Value
	1, // 6: manifest.Manifest.FilesEntry.value:type_name -> manifest.FileManifest
	7, // [7:7] is the sub-list for method output_type
	7, // [7:7] is the sub-list for method input_type
	7, // [7:7] is the sub-list for extension type_name
	7, // [7:7] is the sub-list for extension extendee
	0, // [0:7] is the sub-list for field type_name
}

func init() { file_manifest_proto_init() }
func file_manifest_proto_init() {
	if File_manifest_proto != nil {
		return
	}
	type x struct{}
	out := protoimpl.TypeBuilder{
		File: protoimpl.DescBuilder{
			GoPackagePath: reflect.TypeOf(x{}).PkgPath(),
			RawDescriptor: file_manifest_proto_rawDesc,
			NumEnums:      0,
			NumMessages:   6,
			NumExtensions: 0,
			NumServices:   0,
		},
		GoTypes:           file_manifest_proto_goTypes,
		DependencyIndexes: file_manifest_proto_depIdxs,
		MessageInfos:      file_manifest_proto_msgTypes,
	}.Build()
	File_manifest_proto = out.File
	file_manifest_proto_rawDesc = nil
	file_manifest_proto_goTypes = nil
	file_manifest_proto_depIdxs = nil
}
