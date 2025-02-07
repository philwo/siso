// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Code generated by protoc-gen-go. DO NOT EDIT.
// versions:
// 	protoc-gen-go v1.36.5
// 	protoc        v5.29.3
// source: state.proto

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

type FileID struct {
	state         protoimpl.MessageState `protogen:"open.v1"`
	ModTime       int64                  `protobuf:"varint,1,opt,name=mod_time,json=modTime,proto3" json:"mod_time,omitempty"` // unix nano sec.
	unknownFields protoimpl.UnknownFields
	sizeCache     protoimpl.SizeCache
}

func (x *FileID) Reset() {
	*x = FileID{}
	mi := &file_state_proto_msgTypes[0]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *FileID) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*FileID) ProtoMessage() {}

func (x *FileID) ProtoReflect() protoreflect.Message {
	mi := &file_state_proto_msgTypes[0]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use FileID.ProtoReflect.Descriptor instead.
func (*FileID) Descriptor() ([]byte, []int) {
	return file_state_proto_rawDescGZIP(), []int{0}
}

func (x *FileID) GetModTime() int64 {
	if x != nil {
		return x.ModTime
	}
	return 0
}

type Digest struct {
	state         protoimpl.MessageState `protogen:"open.v1"`
	Hash          string                 `protobuf:"bytes,1,opt,name=hash,proto3" json:"hash,omitempty"`
	SizeBytes     int64                  `protobuf:"varint,2,opt,name=size_bytes,json=sizeBytes,proto3" json:"size_bytes,omitempty"`
	unknownFields protoimpl.UnknownFields
	sizeCache     protoimpl.SizeCache
}

func (x *Digest) Reset() {
	*x = Digest{}
	mi := &file_state_proto_msgTypes[1]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *Digest) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*Digest) ProtoMessage() {}

func (x *Digest) ProtoReflect() protoreflect.Message {
	mi := &file_state_proto_msgTypes[1]
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
	return file_state_proto_rawDescGZIP(), []int{1}
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

type Entry struct {
	state        protoimpl.MessageState `protogen:"open.v1"`
	Id           *FileID                `protobuf:"bytes,1,opt,name=id,proto3" json:"id,omitempty"`
	Name         string                 `protobuf:"bytes,2,opt,name=name,proto3" json:"name,omitempty"`
	Digest       *Digest                `protobuf:"bytes,3,opt,name=digest,proto3" json:"digest,omitempty"`
	IsExecutable bool                   `protobuf:"varint,4,opt,name=is_executable,json=isExecutable,proto3" json:"is_executable,omitempty"`
	// target is symlink target.
	Target string `protobuf:"bytes,5,opt,name=target,proto3" json:"target,omitempty"`
	// action, cmd that generated this file.
	CmdHash       []byte  `protobuf:"bytes,6,opt,name=cmd_hash,json=cmdHash,proto3" json:"cmd_hash,omitempty"`
	Action        *Digest `protobuf:"bytes,7,opt,name=action,proto3" json:"action,omitempty"`
	UpdatedTime   int64   `protobuf:"varint,8,opt,name=updated_time,json=updatedTime,proto3" json:"updated_time,omitempty"` // unix nano sec.
	unknownFields protoimpl.UnknownFields
	sizeCache     protoimpl.SizeCache
}

func (x *Entry) Reset() {
	*x = Entry{}
	mi := &file_state_proto_msgTypes[2]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *Entry) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*Entry) ProtoMessage() {}

func (x *Entry) ProtoReflect() protoreflect.Message {
	mi := &file_state_proto_msgTypes[2]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use Entry.ProtoReflect.Descriptor instead.
func (*Entry) Descriptor() ([]byte, []int) {
	return file_state_proto_rawDescGZIP(), []int{2}
}

func (x *Entry) GetId() *FileID {
	if x != nil {
		return x.Id
	}
	return nil
}

func (x *Entry) GetName() string {
	if x != nil {
		return x.Name
	}
	return ""
}

func (x *Entry) GetDigest() *Digest {
	if x != nil {
		return x.Digest
	}
	return nil
}

func (x *Entry) GetIsExecutable() bool {
	if x != nil {
		return x.IsExecutable
	}
	return false
}

func (x *Entry) GetTarget() string {
	if x != nil {
		return x.Target
	}
	return ""
}

func (x *Entry) GetCmdHash() []byte {
	if x != nil {
		return x.CmdHash
	}
	return nil
}

func (x *Entry) GetAction() *Digest {
	if x != nil {
		return x.Action
	}
	return nil
}

func (x *Entry) GetUpdatedTime() int64 {
	if x != nil {
		return x.UpdatedTime
	}
	return 0
}

type State struct {
	state   protoimpl.MessageState `protogen:"open.v1"`
	Entries []*Entry               `protobuf:"bytes,1,rep,name=entries,proto3" json:"entries,omitempty"`
	// last checked token for fsmonitor (e.g. watchman)
	LastChecked   string `protobuf:"bytes,2,opt,name=last_checked,json=lastChecked,proto3" json:"last_checked,omitempty"`
	unknownFields protoimpl.UnknownFields
	sizeCache     protoimpl.SizeCache
}

func (x *State) Reset() {
	*x = State{}
	mi := &file_state_proto_msgTypes[3]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *State) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*State) ProtoMessage() {}

func (x *State) ProtoReflect() protoreflect.Message {
	mi := &file_state_proto_msgTypes[3]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use State.ProtoReflect.Descriptor instead.
func (*State) Descriptor() ([]byte, []int) {
	return file_state_proto_rawDescGZIP(), []int{3}
}

func (x *State) GetEntries() []*Entry {
	if x != nil {
		return x.Entries
	}
	return nil
}

func (x *State) GetLastChecked() string {
	if x != nil {
		return x.LastChecked
	}
	return ""
}

var File_state_proto protoreflect.FileDescriptor

var file_state_proto_rawDesc = string([]byte{
	0x0a, 0x0b, 0x73, 0x74, 0x61, 0x74, 0x65, 0x2e, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x12, 0x0b, 0x73,
	0x69, 0x73, 0x6f, 0x2e, 0x68, 0x61, 0x73, 0x68, 0x66, 0x73, 0x22, 0x23, 0x0a, 0x06, 0x46, 0x69,
	0x6c, 0x65, 0x49, 0x44, 0x12, 0x19, 0x0a, 0x08, 0x6d, 0x6f, 0x64, 0x5f, 0x74, 0x69, 0x6d, 0x65,
	0x18, 0x01, 0x20, 0x01, 0x28, 0x03, 0x52, 0x07, 0x6d, 0x6f, 0x64, 0x54, 0x69, 0x6d, 0x65, 0x22,
	0x3b, 0x0a, 0x06, 0x44, 0x69, 0x67, 0x65, 0x73, 0x74, 0x12, 0x12, 0x0a, 0x04, 0x68, 0x61, 0x73,
	0x68, 0x18, 0x01, 0x20, 0x01, 0x28, 0x09, 0x52, 0x04, 0x68, 0x61, 0x73, 0x68, 0x12, 0x1d, 0x0a,
	0x0a, 0x73, 0x69, 0x7a, 0x65, 0x5f, 0x62, 0x79, 0x74, 0x65, 0x73, 0x18, 0x02, 0x20, 0x01, 0x28,
	0x03, 0x52, 0x09, 0x73, 0x69, 0x7a, 0x65, 0x42, 0x79, 0x74, 0x65, 0x73, 0x22, 0x95, 0x02, 0x0a,
	0x05, 0x45, 0x6e, 0x74, 0x72, 0x79, 0x12, 0x23, 0x0a, 0x02, 0x69, 0x64, 0x18, 0x01, 0x20, 0x01,
	0x28, 0x0b, 0x32, 0x13, 0x2e, 0x73, 0x69, 0x73, 0x6f, 0x2e, 0x68, 0x61, 0x73, 0x68, 0x66, 0x73,
	0x2e, 0x46, 0x69, 0x6c, 0x65, 0x49, 0x44, 0x52, 0x02, 0x69, 0x64, 0x12, 0x12, 0x0a, 0x04, 0x6e,
	0x61, 0x6d, 0x65, 0x18, 0x02, 0x20, 0x01, 0x28, 0x09, 0x52, 0x04, 0x6e, 0x61, 0x6d, 0x65, 0x12,
	0x2b, 0x0a, 0x06, 0x64, 0x69, 0x67, 0x65, 0x73, 0x74, 0x18, 0x03, 0x20, 0x01, 0x28, 0x0b, 0x32,
	0x13, 0x2e, 0x73, 0x69, 0x73, 0x6f, 0x2e, 0x68, 0x61, 0x73, 0x68, 0x66, 0x73, 0x2e, 0x44, 0x69,
	0x67, 0x65, 0x73, 0x74, 0x52, 0x06, 0x64, 0x69, 0x67, 0x65, 0x73, 0x74, 0x12, 0x23, 0x0a, 0x0d,
	0x69, 0x73, 0x5f, 0x65, 0x78, 0x65, 0x63, 0x75, 0x74, 0x61, 0x62, 0x6c, 0x65, 0x18, 0x04, 0x20,
	0x01, 0x28, 0x08, 0x52, 0x0c, 0x69, 0x73, 0x45, 0x78, 0x65, 0x63, 0x75, 0x74, 0x61, 0x62, 0x6c,
	0x65, 0x12, 0x16, 0x0a, 0x06, 0x74, 0x61, 0x72, 0x67, 0x65, 0x74, 0x18, 0x05, 0x20, 0x01, 0x28,
	0x09, 0x52, 0x06, 0x74, 0x61, 0x72, 0x67, 0x65, 0x74, 0x12, 0x19, 0x0a, 0x08, 0x63, 0x6d, 0x64,
	0x5f, 0x68, 0x61, 0x73, 0x68, 0x18, 0x06, 0x20, 0x01, 0x28, 0x0c, 0x52, 0x07, 0x63, 0x6d, 0x64,
	0x48, 0x61, 0x73, 0x68, 0x12, 0x2b, 0x0a, 0x06, 0x61, 0x63, 0x74, 0x69, 0x6f, 0x6e, 0x18, 0x07,
	0x20, 0x01, 0x28, 0x0b, 0x32, 0x13, 0x2e, 0x73, 0x69, 0x73, 0x6f, 0x2e, 0x68, 0x61, 0x73, 0x68,
	0x66, 0x73, 0x2e, 0x44, 0x69, 0x67, 0x65, 0x73, 0x74, 0x52, 0x06, 0x61, 0x63, 0x74, 0x69, 0x6f,
	0x6e, 0x12, 0x21, 0x0a, 0x0c, 0x75, 0x70, 0x64, 0x61, 0x74, 0x65, 0x64, 0x5f, 0x74, 0x69, 0x6d,
	0x65, 0x18, 0x08, 0x20, 0x01, 0x28, 0x03, 0x52, 0x0b, 0x75, 0x70, 0x64, 0x61, 0x74, 0x65, 0x64,
	0x54, 0x69, 0x6d, 0x65, 0x22, 0x58, 0x0a, 0x05, 0x53, 0x74, 0x61, 0x74, 0x65, 0x12, 0x2c, 0x0a,
	0x07, 0x65, 0x6e, 0x74, 0x72, 0x69, 0x65, 0x73, 0x18, 0x01, 0x20, 0x03, 0x28, 0x0b, 0x32, 0x12,
	0x2e, 0x73, 0x69, 0x73, 0x6f, 0x2e, 0x68, 0x61, 0x73, 0x68, 0x66, 0x73, 0x2e, 0x45, 0x6e, 0x74,
	0x72, 0x79, 0x52, 0x07, 0x65, 0x6e, 0x74, 0x72, 0x69, 0x65, 0x73, 0x12, 0x21, 0x0a, 0x0c, 0x6c,
	0x61, 0x73, 0x74, 0x5f, 0x63, 0x68, 0x65, 0x63, 0x6b, 0x65, 0x64, 0x18, 0x02, 0x20, 0x01, 0x28,
	0x09, 0x52, 0x0b, 0x6c, 0x61, 0x73, 0x74, 0x43, 0x68, 0x65, 0x63, 0x6b, 0x65, 0x64, 0x42, 0x1f,
	0x5a, 0x1d, 0x69, 0x6e, 0x66, 0x72, 0x61, 0x2f, 0x62, 0x75, 0x69, 0x6c, 0x64, 0x2f, 0x73, 0x69,
	0x73, 0x6f, 0x2f, 0x68, 0x61, 0x73, 0x68, 0x66, 0x73, 0x2f, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x62,
	0x06, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x33,
})

var (
	file_state_proto_rawDescOnce sync.Once
	file_state_proto_rawDescData []byte
)

func file_state_proto_rawDescGZIP() []byte {
	file_state_proto_rawDescOnce.Do(func() {
		file_state_proto_rawDescData = protoimpl.X.CompressGZIP(unsafe.Slice(unsafe.StringData(file_state_proto_rawDesc), len(file_state_proto_rawDesc)))
	})
	return file_state_proto_rawDescData
}

var file_state_proto_msgTypes = make([]protoimpl.MessageInfo, 4)
var file_state_proto_goTypes = []any{
	(*FileID)(nil), // 0: siso.hashfs.FileID
	(*Digest)(nil), // 1: siso.hashfs.Digest
	(*Entry)(nil),  // 2: siso.hashfs.Entry
	(*State)(nil),  // 3: siso.hashfs.State
}
var file_state_proto_depIdxs = []int32{
	0, // 0: siso.hashfs.Entry.id:type_name -> siso.hashfs.FileID
	1, // 1: siso.hashfs.Entry.digest:type_name -> siso.hashfs.Digest
	1, // 2: siso.hashfs.Entry.action:type_name -> siso.hashfs.Digest
	2, // 3: siso.hashfs.State.entries:type_name -> siso.hashfs.Entry
	4, // [4:4] is the sub-list for method output_type
	4, // [4:4] is the sub-list for method input_type
	4, // [4:4] is the sub-list for extension type_name
	4, // [4:4] is the sub-list for extension extendee
	0, // [0:4] is the sub-list for field type_name
}

func init() { file_state_proto_init() }
func file_state_proto_init() {
	if File_state_proto != nil {
		return
	}
	type x struct{}
	out := protoimpl.TypeBuilder{
		File: protoimpl.DescBuilder{
			GoPackagePath: reflect.TypeOf(x{}).PkgPath(),
			RawDescriptor: unsafe.Slice(unsafe.StringData(file_state_proto_rawDesc), len(file_state_proto_rawDesc)),
			NumEnums:      0,
			NumMessages:   4,
			NumExtensions: 0,
			NumServices:   0,
		},
		GoTypes:           file_state_proto_goTypes,
		DependencyIndexes: file_state_proto_depIdxs,
		MessageInfos:      file_state_proto_msgTypes,
	}.Build()
	File_state_proto = out.File
	file_state_proto_goTypes = nil
	file_state_proto_depIdxs = nil
}
