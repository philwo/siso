// Copyright 2023 The Chromium Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package digest handles content digests of remote executon API.
//
// You can find the Digest proto in REAPI here:
// https://github.com/bazelbuild/remote-apis/blob/c1c1ad2c97ed18943adb55f06657440daa60d833/build/bazel/remote/execution/v2/remote_execution.proto#L633
package digest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"

	"github.com/bazelbuild/remote-apis-sdks/go/pkg/digest"
	"google.golang.org/protobuf/proto"

	"infra/build/siso/o11y/iometrics"
	"infra/build/siso/reapi/retry"
)

// TODO: remove this.
type Digest = digest.Digest

// Source is the interface that opens a data source.
// It can be remote or local source.
// If this interface is implemented based on gRPC streaming for remote sources,
// the caller may need to retry Open/Read/Close in addition.
type Source interface {
	// Open returns io.ReadCloser of the source.
	Open(context.Context) (io.ReadCloser, error)

	// String returns the name of the data source.
	String() string
}

// Data is a data instance that consists of Digest and Source.
// TODO(b/268407930): it may be possible to be merged with Source.
type Data struct {
	digest digest.Digest
	source Source
}

// NewData creates a Data from source and digest.
func NewData(src Source, d digest.Digest) Data {
	return Data{
		digest: d,
		source: src,
	}
}

// IsZero returns true when the Data is zero value struct.
func (d Data) IsZero() bool {
	return d.digest.Hash == ""
}

// Digest returns the Digest of the data.
func (d Data) Digest() digest.Digest {
	return d.digest
}

// Open opens the data source.
func (d Data) Open(ctx context.Context) (io.ReadCloser, error) {
	return d.source.Open(ctx)
}

// String returns the digest and the source in string format.
func (d Data) String() string {
	return fmt.Sprintf("%v %v", d.digest, d.source)
}

// DataToBytes returns byte values from a Data.
// Note that it reads all content. It should not be used for large blob.
func DataToBytes(ctx context.Context, d Data) ([]byte, error) {
	var buf []byte
	err := retry.Do(ctx, func() error {
		f, err := d.Open(ctx)
		if err != nil {
			return err
		}
		defer f.Close()
		buf, err = io.ReadAll(f)
		return err
	})
	return buf, err

}

// FromProtoMessage creates Data from proto message.
func FromProtoMessage(m proto.Message) (Data, error) {
	b, err := proto.Marshal(m)
	if err != nil {
		return Data{}, err
	}
	return FromBytes(fmt.Sprintf("%T", m), b), nil
}

// FromBytes creates data from raw byte values.
func FromBytes(name string, b []byte) Data {
	return Data{
		digest: digest.NewFromBlob(b),
		source: byteSource{name: name, b: b},
	}
}

// byteSource implements Source for in-memory source with raw byte values.
type byteSource struct {
	name string
	b    []byte
}

func (b byteSource) Open(ctx context.Context) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(b.b)), nil
}

func (b byteSource) String() string {
	return b.name
}

// FromLocalFile creates Data from local file source.
func FromLocalFile(ctx context.Context, src LocalFileSource) (Data, error) {
	f, err := src.Open(ctx)
	if err != nil {
		return Data{}, err
	}
	defer f.Close()
	d, err := digest.NewFromReader(f)
	if err != nil {
		return Data{}, err
	}
	return Data{
		digest: d,
		source: src,
	}, nil
}

// LocalFileSource is a source for local file.
type LocalFileSource struct {
	Fname     string
	IOMetrics *iometrics.IOMetrics
}

type localFile struct {
	*os.File
	m *iometrics.IOMetrics
	n int
}

// Read reads the content of the local file.
func (f *localFile) Read(buf []byte) (int, error) {
	n, err := f.File.Read(buf)
	f.n += n
	return n, err
}

// Close closes the local file.
func (f *localFile) Close() error {
	err := f.File.Close()
	if f.m != nil {
		f.m.ReadDone(f.n, err)
	}
	return err
}

// Open opens local file.
func (s LocalFileSource) Open(ctx context.Context) (io.ReadCloser, error) {
	r, err := os.Open(s.Fname)
	return &localFile{File: r, m: s.IOMetrics}, err
}

// String returns the source name with "file://" prefix.
func (s LocalFileSource) String() string {
	return fmt.Sprintf("file://%s", s.Fname)
}
