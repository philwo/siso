// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package bytestreamio provides io interfaces on bytestream service.
package bytestreamio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"

	pb "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"go.chromium.org/infra/build/siso/o11y/clog"
)

// Open opens a reader on the bytestream for the resourceName.
// ctx will be used until the reader is closed.
func Open(ctx context.Context, c pb.ByteStreamClient, resourceName string) (*Reader, error) {
	rd, err := c.Read(ctx, &pb.ReadRequest{
		ResourceName: resourceName,
	})
	if err != nil {
		return nil, err
	}
	return &Reader{
		rd: rd,
	}, nil
}

// Reader is a reader on bytestream and implements io.Reader.
type Reader struct {
	rd  pb.ByteStream_ReadClient
	buf []byte
	// size of data already read from the bytestream.
	size int64
}

// Read reads data from bytestream.
// The maximum data chunk size would be determined by server side.
func (r *Reader) Read(buf []byte) (int, error) {
	if r.rd == nil {
		return 0, errors.New("failed to read: bad Reader")
	}
	if len(r.buf) > 0 {
		n := copy(buf, r.buf)
		r.buf = r.buf[n:]
		r.size += int64(n)
		return n, nil
	}
	resp, err := r.rd.Recv()
	if err != nil {
		return 0, err
	}
	// resp.Data may be empty.
	r.buf = resp.Data
	n := copy(buf, r.buf)
	r.buf = r.buf[n:]
	r.size += int64(n)
	return n, nil
}

// Size reports read size by Read.
func (r *Reader) Size() int64 {
	return r.size
}

// Create creates a writer on the bytestream for resourceName.
// ctx will be used until the rriter is closed.
func Create(ctx context.Context, c pb.ByteStreamClient, resourceName, name string) (*Writer, error) {
	sizeStr := path.Base(resourceName)
	size, err := strconv.ParseInt(sizeStr, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("bad size in resource name %q: %v", resourceName, err)
	}
	wr, err := c.Write(ctx)
	if err != nil {
		return nil, err
	}
	return &Writer{
		name:    name,
		resname: resourceName,
		size:    size,
		wr:      wr,
	}, nil
}

// Writer is a writer on bytestream, and implemnets io.Writer.
type Writer struct {
	name    string // data source name
	resname string // resource name for upload destination
	size    int64
	wr      pb.ByteStream_WriteClient
	offset  int64

	// bytestream server will accept blobs by partial upload if
	// the same blobs are already uploaded by io.EOF of Send.
	// then, ok becomes true and we don't need to Send the rest of
	// data, so Write just returns success.  Close issues
	// CloseAndRecv and doesn't check offset.
	ok bool
}

// Write writes data to bytestream.
// The maximum data chunk size would be determined by server side,
// so don't pass larger chunk than maximum data chunk size.
func (w *Writer) Write(buf []byte) (int, error) {
	if w.wr == nil {
		return 0, errors.New("failed to write: bad Writer")
	}
	if w.ok {
		return len(buf), nil
	}
	err := w.wr.Send(&pb.WriteRequest{
		ResourceName: w.resname,
		WriteOffset:  w.offset,
		Data:         buf,
	})
	if err == io.EOF {
		// the blob already stored in CAS.
		w.ok = true
		clog.Infof(w.wr.Context(), "bytestream write %s for %s got EOF at %d: %v", w.resname, w.name, w.offset, err)
		return len(buf), nil
	}
	if err != nil {
		return 0, fmt.Errorf("failed to send for %s: %w", w.name, err)
	}
	w.offset += int64(len(buf))
	return len(buf), nil
}

// Close closes the writer.
func (w *Writer) Close() error {
	if w.wr == nil {
		return errors.New("bad Writer")
	}
	if !w.ok {
		// The service will not view the resource as 'complete'
		// until the client has sent a 'WriteRequest' with 'FinishWrite'
		// set to 'true'.
		err := w.wr.Send(&pb.WriteRequest{
			ResourceName: w.resname,
			WriteOffset:  w.offset,
			FinishWrite:  true,
			// The client may leave 'data' empty.
		})
		if err != nil {
			return fmt.Errorf("failed to finish for %s: %w", w.name, err)
		}
	}
	res, err := w.wr.CloseAndRecv()
	if err != nil {
		return fmt.Errorf("failed to close for %s: %w", w.name, err)
	}
	// in case compressed-blobs, res.CommittedSize != w.offset.
	// since w.offset is compressed size.
	// res.CommittedSize is original data size, so it must match with
	// size in resource name (i.e. size_bytes in digest).
	if res.CommittedSize != w.size {
		return status.Errorf(codes.Internal, "unexpected committedSize: %d != %d for %s", res.CommittedSize, w.size, w.name)
	}
	return nil
}
