// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package reapi

import (
	"compress/flate"
	"context"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	log "github.com/golang/glog"
	"github.com/klauspost/compress/zstd"
	bpb "google.golang.org/genproto/googleapis/bytestream"

	"infra/build/siso/o11y/clog"
	"infra/build/siso/o11y/trace"
	"infra/build/siso/reapi/bytestreamio"
	"infra/build/siso/reapi/digest"
	"infra/build/siso/reapi/retry"
)

const (
	// bytestreamReadThreshold is the threshold that decides whether to use BatchReadBlobs or ByteStream API.
	bytestreamReadThreshold = 2 * 1024 * 1024
)

func (c *Client) useCompressedBlob(d digest.Digest) bool {
	if c.opt.CompressedBlob <= 0 {
		return false
	}
	return d.SizeBytes >= c.opt.CompressedBlob
}

func (c *Client) getCompressor() rpb.Compressor_Value {
	if len(c.capabilities.CacheCapabilities.SupportedCompressors) == 0 {
		// No compressor support.
		return rpb.Compressor_IDENTITY
	}
	// always use the first supported compressor for now.
	return c.capabilities.CacheCapabilities.SupportedCompressors[0]
}

// resourceName constructs a resource name for the blob identified by the digest.
// For uncompressed blob. the format is
//
//	`{instance_name}/blobs/{hash}/{size}`
//
// For compressed blob, the format is
//
//	`{instance_name}/compressed-blobs/{compressor}/{uncompressed_hash}/{uncompressed_size}`
//
// See also the API document.
// https://github.com/bazelbuild/remote-apis/blob/64cc5e9e422c93e1d7f0545a146fd84fcc0e8b47/build/bazel/remote/execution/v2/remote_execution.proto#L285-L292
func (c *Client) resourceName(d digest.Digest) string {
	if c.useCompressedBlob(d) {
		return path.Join(c.opt.Instance, "compressed-blobs",
			strings.ToLower(c.getCompressor().String()),
			d.Hash, strconv.FormatInt(d.SizeBytes, 10))
	}
	return path.Join(c.opt.Instance, "blobs", d.Hash, strconv.FormatInt(d.SizeBytes, 10))
}

// newDecoder returns a decoder to uncompress blob.
// For uncompressed blob, it returns a nop closer.
func (c *Client) newDecoder(r io.Reader, d digest.Digest) (io.ReadCloser, error) {
	if c.useCompressedBlob(d) {
		switch comp := c.getCompressor(); comp {
		case rpb.Compressor_ZSTD:
			rd, err := zstd.NewReader(r)
			return rd.IOReadCloser(), err
		case rpb.Compressor_DEFLATE:
			return flate.NewReader(r), nil
		default:
			return nil, fmt.Errorf("unsupported compressor %q", comp)
		}
	}
	return io.NopCloser(r), nil
}

// Get fetches the content of blob from CAS by digest.
// For small blobs, it uses BatchReadBlobs.
// For large blobs, it uses Read method of the ByteStream API
func (c *Client) Get(ctx context.Context, d digest.Digest, name string) ([]byte, error) {
	if c == nil {
		return nil, fmt.Errorf("reapi is not configured")
	}
	if d.SizeBytes == 0 {
		return nil, nil
	}

	ctx, span := trace.NewSpan(ctx, "reapi-get")
	defer span.Close(nil)
	span.SetAttr("sizebytes", d.SizeBytes)

	if d.SizeBytes < bytestreamReadThreshold {
		return c.getWithBatchReadBlobs(ctx, d, name)
	}
	return c.getWithByteStream(ctx, d)
}

// getWithBatchReadBlobs fetches the content of blob using BatchReadBlobs rpc of CAS.
func (c *Client) getWithBatchReadBlobs(ctx context.Context, d digest.Digest, name string) ([]byte, error) {
	casClient := rpb.NewContentAddressableStorageClient(c.conn)
	resp, err := casClient.BatchReadBlobs(ctx, &rpb.BatchReadBlobsRequest{
		InstanceName: c.opt.Instance,
		Digests:      []*rpb.Digest{d.Proto()},
	})
	if err != nil {
		c.m.ReadDone(0, err)
		return nil, fmt.Errorf("failed to read blobs %s for %s: %w", d, name, err)
	}
	if len(resp.Responses) != 1 {
		c.m.ReadDone(0, err)
		return nil, fmt.Errorf("failed to read blobs %s for %s: responses=%d", d, name, len(resp.Responses))
	}
	c.m.ReadDone(len(resp.Responses[0].Data), err)
	if int64(len(resp.Responses[0].Data)) != d.SizeBytes {
		return nil, fmt.Errorf("failed to read blobs %s for %s: size mismatch got=%d", d, name, len(resp.Responses[0].Data))
	}
	return resp.Responses[0].Data, nil
}

// getWithByteStream fetches the content of blob using the ByteStream API
func (c *Client) getWithByteStream(ctx context.Context, d digest.Digest) ([]byte, error) {
	resourceName := c.resourceName(d)
	if log.V(1) {
		clog.Infof(ctx, "get %s", resourceName)
	}
	var buf []byte
	err := retry.Do(ctx, func() error {
		r, err := bytestreamio.Open(ctx, bpb.NewByteStreamClient(c.conn), resourceName)
		if err != nil {
			c.m.ReadDone(0, err)
			return err
		}
		rd, err := c.newDecoder(r, d)
		if err != nil {
			c.m.ReadDone(0, err)
			return err
		}
		defer rd.Close()
		buf = make([]byte, d.SizeBytes)
		n, err := io.ReadFull(rd, buf)
		c.m.ReadDone(n, err)
		if err != nil {
			return err
		}
		return nil
	})
	return buf, err
}
