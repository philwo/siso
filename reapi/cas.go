// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package reapi

import (
	"compress/flate"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	log "github.com/golang/glog"
	"github.com/google/uuid"
	"github.com/klauspost/compress/zstd"
	bpb "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"infra/build/siso/o11y/clog"
	"infra/build/siso/o11y/trace"
	"infra/build/siso/reapi/bytestreamio"
	"infra/build/siso/reapi/digest"
	"infra/build/siso/reapi/retry"
)

const (
	// bytestreamReadThreshold is the threshold that decides whether to use BatchReadBlobs or ByteStream API.
	bytestreamReadThreshold = 2 * 1024 * 1024

	// defaultBatchUpdateByteLimit is bytes limit for cas BatchUpdateBlobs.
	defaultBatchUpdateByteLimit = 4 * 1024 * 1024

	// batchBlobUploadLimit is max number of blobs in BatchUpdateBlobs.
	batchBlobUploadLimit = 1000
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

// resourceName constructs a resource name for reading the blob identified by the digest.
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

// Missing returns digests of missing blobs.
func (c *Client) Missing(ctx context.Context, blobs []digest.Digest) ([]digest.Digest, error) {
	blobspb := make([]*rpb.Digest, 0, len(blobs))
	for _, b := range blobs {
		blobspb = append(blobspb, b.Proto())
	}
	cas := rpb.NewContentAddressableStorageClient(c.conn)
	resp, err := cas.FindMissingBlobs(ctx, &rpb.FindMissingBlobsRequest{
		InstanceName: c.opt.Instance,
		BlobDigests:  blobspb,
	})
	c.m.OpsDone(err)
	if err != nil {
		return nil, err
	}
	ret := make([]digest.Digest, 0, len(resp.GetMissingBlobDigests()))
	for _, b := range resp.GetMissingBlobDigests() {
		ret = append(ret, digest.FromProto(b))
	}
	return ret, nil
}

// UploadAll uploads all blobs specified in ds that are still missing in the CAS.
func (c *Client) UploadAll(ctx context.Context, ds *digest.Store) (int, error) {
	if c.conn == nil {
		return 0, status.Error(codes.FailedPrecondition, "conn is not configured")
	}
	ctx, span := trace.NewSpan(ctx, "upload-all")
	defer span.Close(nil)
	blobs := ds.List()
	span.SetAttr("blobs", len(blobs))
	missings, err := c.Missing(ctx, blobs)
	if err != nil {
		return 0, err
	}
	clog.Infof(ctx, "upload %d -> missing %d", len(blobs), len(missings))
	span.SetAttr("missings", len(missings))
	return c.Upload(ctx, ds, missings)
}

var (
	errBlobNotInReq = errors.New("blob not in request")
)

type missingBlob struct {
	Digest digest.Digest
	Err    error
}

type missingError struct {
	Blobs []missingBlob
}

func (e missingError) Error() string {
	return fmt.Sprintf("missing %d blobs", len(e.Blobs))
}

// Upload uploads blobs in digest stores.
func (c *Client) Upload(ctx context.Context, ds *digest.Store, blobs []digest.Digest) (int, error) {
	if len(blobs) == 0 {
		return 0, nil
	}

	ctx, span := trace.NewSpan(ctx, "upload")
	defer span.Close(nil)

	byteLimit := int64(defaultBatchUpdateByteLimit)
	if max := c.capabilities.GetCacheCapabilities().GetMaxBatchTotalSizeBytes(); max > 0 {
		byteLimit = max
	}

	// Separate small blobs and large blobs because they are going to use different RPCs.
	smalls, larges := separateBlobs(c.opt.Instance, blobs, byteLimit)
	clog.Infof(ctx, "upload by batch %d out of %d", len(smalls), len(blobs))
	span.SetAttr("small", len(smalls))
	span.SetAttr("large", len(larges))

	// Upload small blobs with BatchUpdateBlobs rpc.
	missingBlobs, err := c.uploadWithBatchUpdateBlobs(ctx, smalls, ds, byteLimit)
	if err != nil {
		return 0, err
	}
	missing := missingError{Blobs: missingBlobs}

	// Upload large blobs with ByteStream API.
	missingBlobs = c.uploadWithByteStream(ctx, larges, ds)
	missing.Blobs = append(missing.Blobs, missingBlobs...)

	if len(missing.Blobs) > 0 {
		return len(blobs) - len(missing.Blobs), missing
	}
	return len(blobs), nil
}

// separateBlobs separates blobs to two groups.
// One group is for small blobs that can fit in BatchUpdateBlobsRequest, and the other is for large blobs.
// TODO(b/273884978): simplify and optimize the code.
func separateBlobs(instance string, blobs []digest.Digest, byteLimit int64) (smalls, larges []digest.Digest) {
	if len(blobs) == 0 {
		return nil, nil
	}
	sort.Slice(blobs, func(i, j int) bool {
		return blobs[i].SizeBytes < blobs[j].SizeBytes
	})
	maxSizeBytes := blobs[len(blobs)-1].SizeBytes
	if maxSizeBytes > byteLimit {
		maxSizeBytes = byteLimit
	}
	// Prepare a dummy request message to calculate the size of the BatchUpdateBlobsRequest accurately.
	dummyReq := &rpb.BatchUpdateBlobsRequest{
		InstanceName: instance,
		Requests: []*rpb.BatchUpdateBlobsRequest_Request{
			{Data: make([]byte, 0, maxSizeBytes)},
		},
	}
	i := sort.Search(len(blobs), func(i int) bool {
		if blobs[i].SizeBytes >= byteLimit {
			return true
		}
		// When the BatchUpdateBlobsRequest with the single blob already exceeds the size limit.
		// It can't send the blob via BatchUpdateBlobsRequest.
		dummyReq.Requests[0].Digest = blobs[i].Proto()
		dummyReq.Requests[0].Data = dummyReq.Requests[0].Data[:blobs[i].SizeBytes]
		return int64(proto.Size(dummyReq)) >= byteLimit
	})
	if i < len(blobs) {
		return blobs[:i], blobs[i:]
	}
	return blobs, nil
}

// uploadWithBatchUpdateBlobs uploads blobs using BatchUpdateBlobs RPC.
// The blobs will be bundled into multiple batches that fit in the size limit.
func (c *Client) uploadWithBatchUpdateBlobs(ctx context.Context, digests []digest.Digest, ds *digest.Store, byteLimit int64) ([]missingBlob, error) {
	blobReqs, missingBlobs := lookupBlobsInStore(ctx, digests, ds)

	// Bundle the blobs to multiple batch requests.
	batchReqs := createBatchUpdateBlobsRequests(c.opt.Instance, blobReqs, byteLimit)
	casClient := rpb.NewContentAddressableStorageClient(c.conn)

	// TODO(b/273884978): It may be worth trying to send the batch requests in parallel.
	for _, batchReq := range batchReqs {
		n := 0
		for _, r := range batchReq.Requests {
			n += int(r.Digest.SizeBytes)
		}
		uploaded := false
		for !uploaded {
			var batchResp *rpb.BatchUpdateBlobsResponse
			batchResp, err := casClient.BatchUpdateBlobs(ctx, batchReq)
			if err != nil {
				c.m.WriteDone(0, err)
				return nil, status.Errorf(status.Code(err), "batch update blobs: %v", err)
			}
			for _, res := range batchResp.Responses {
				blob := digest.FromProto(res.Digest)
				data, ok := ds.Get(blob)
				if !ok {
					clog.Warningf(ctx, "Not found %s in store", blob)
					missingBlobs = append(missingBlobs, missingBlob{
						Digest: blob,
						Err:    errBlobNotInReq,
					})
					continue
				}
				st := status.FromProto(res.GetStatus())
				if st.Code() != codes.OK {
					clog.Warningf(ctx, "Failed to batch-update %s: %v", data, st)
					err := status.Errorf(st.Code(), "batch update blobs: %v", res.Status)
					missingBlobs = append(missingBlobs, missingBlob{
						Digest: blob,
						Err:    err,
					})
					c.m.WriteDone(int(res.Digest.SizeBytes), err)
				} else {
					clog.Infof(ctx, "uploaded in batch: %s", data)
					c.m.WriteDone(int(res.Digest.SizeBytes), nil)
				}
			}
			uploaded = true
			clog.Infof(ctx, "upload by batch %d blobs (missing:%d)", len(batchReq.Requests), len(missingBlobs))
		}
	}
	return missingBlobs, nil
}

func lookupBlobsInStore(ctx context.Context, blobs []digest.Digest, ds *digest.Store) ([]*rpb.BatchUpdateBlobsRequest_Request, []missingBlob) {
	var wg sync.WaitGroup
	type res struct {
		err error
		req *rpb.BatchUpdateBlobsRequest_Request
	}
	results := make([]res, len(blobs))
	for i := range blobs {
		wg.Add(1)
		go func(blob digest.Digest, result *res) {
			defer wg.Done()
			data, ok := ds.Get(blob)
			if !ok {
				result.err = errBlobNotInReq
				return
			}
			b, err := readAll(ctx, data)
			if err != nil {
				result.err = err
				return
			}
			result.req = &rpb.BatchUpdateBlobsRequest_Request{
				Digest: data.Digest().Proto(),
				Data:   b,
			}
		}(blobs[i], &results[i])
	}
	wg.Wait()

	var reqs []*rpb.BatchUpdateBlobsRequest_Request
	var missings []missingBlob
	for i, result := range results {
		blob := blobs[i]
		switch {
		case result.err != nil:
			missings = append(missings, missingBlob{
				Digest: blob,
				Err:    result.err,
			})
		case result.req != nil:
			reqs = append(reqs, result.req)
			continue
		default:
			clog.Errorf(ctx, "lookup of blobs[%d]=%v no error nor req", i, blob)
		}
	}
	return reqs, missings
}

// createBatchUpdateBlobsRequests bundles blobs into multiple batch requests.
func createBatchUpdateBlobsRequests(instance string, blobReqs []*rpb.BatchUpdateBlobsRequest_Request, byteLimit int64) []*rpb.BatchUpdateBlobsRequest {
	var batchReqs []*rpb.BatchUpdateBlobsRequest

	// Initial batch request size without blobs.
	batchReqNoReqsSize := int64(proto.Size(&rpb.BatchUpdateBlobsRequest{InstanceName: instance}))
	size := batchReqNoReqsSize

	lastOffset := 0
	for i := range blobReqs {
		size += int64(proto.Size(&rpb.BatchUpdateBlobsRequest{Requests: blobReqs[i : i+1]}))
		switch {
		case i == len(blobReqs)-1:
			fallthrough
		case i+1 == lastOffset+batchBlobUploadLimit:
			fallthrough
		case byteLimit > 0 && size+int64(proto.Size(&rpb.BatchUpdateBlobsRequest{Requests: blobReqs[i+1 : i+2]})) > byteLimit:
			// When the batch request exceeds the size limit, it starts creating a new batch request.
			batchReqs = append(batchReqs, &rpb.BatchUpdateBlobsRequest{
				InstanceName: instance,
				Requests:     blobReqs[lastOffset : i+1],
			})
			size = batchReqNoReqsSize
			lastOffset = i + 1

		}
	}
	return batchReqs
}

func readAll(ctx context.Context, data digest.Data) ([]byte, error) {
	var buf []byte
	err := retry.Do(ctx, func() error {
		r, err := data.Open(ctx)
		if err != nil {
			return err
		}
		defer r.Close()
		buf, err = io.ReadAll(r)
		return err
	})
	if err != nil {
		return nil, err
	}
	return buf, nil
}

func (c *Client) uploadWithByteStream(ctx context.Context, digests []digest.Digest, ds *digest.Store) []missingBlob {
	clog.Infof(ctx, "upload by streaming %d", len(digests))

	var missingBlobs []missingBlob
	bsClient := bpb.NewByteStreamClient(c.conn)
	for _, d := range digests {
		data, ok := ds.Get(d)
		if !ok {
			clog.Warningf(ctx, "Not found %s in store", d)
			missingBlobs = append(missingBlobs, missingBlob{
				Digest: d,
				Err:    errBlobNotInReq,
			})
			continue
		}
		key := fmt.Sprintf("%s/%d", d.Hash, d.SizeBytes)
		_, err, shared := c.bytestreamSingleflight.Do(key, func() (any, error) {
			err := retry.Do(ctx, func() error {
				rd, err := data.Open(ctx)
				if err != nil {
					return err
				}
				defer rd.Close()
				resourceName := c.uploadResourceName(d)
				if log.V(1) {
					clog.Infof(ctx, "put %s", resourceName)
				}
				wr, err := bytestreamio.Create(ctx, bsClient, c.uploadResourceName(d))
				if err != nil {
					return err
				}
				cwr, err := c.newEncoder(wr, d)
				if err != nil {
					wr.Close()
					return err
				}
				_, err = io.Copy(cwr, rd)
				if err != nil {
					cwr.Close()
					wr.Close()
					return err
				}
				err = cwr.Close()
				if err != nil {
					wr.Close()
					return err
				}
				return wr.Close()
			})
			return nil, err
		})
		if !shared {
			c.m.WriteDone(int(d.SizeBytes), err)
		}
		if err != nil {
			clog.Warningf(ctx, "Failed to stream %s: %v", data, err)
			missingBlobs = append(missingBlobs, missingBlob{
				Digest: d,
				Err:    err,
			})
			continue
		} else {
			clog.Infof(ctx, "uploaded streaming %s shared=%t err=%v", data, shared, err)
		}
	}
	clog.Infof(ctx, "uploaded by streaming %d blobs (missing:%d)", len(digests), len(missingBlobs))

	return missingBlobs
}

// resourceName constructs a resource name for uploading the blob identified by the digest.
// For uncompressed blob. the format is
//
// `{instance_name}/uploads/{uuid}/blobs/{hash}/{size}`
//
// # For compressed blob, the format is
//
// `{instance_name}/uploads/{uuid}/compressed-blobs/{compressor}/{uncompressed_hash}/{uncompressed_size}`
//
// See also the API document.
// https://github.com/bazelbuild/remote-apis/blob/64cc5e9e422c93e1d7f0545a146fd84fcc0e8b47/build/bazel/remote/execution/v2/remote_execution.proto#L211-L239
func (c *Client) uploadResourceName(d digest.Digest) string {
	if c.useCompressedBlob(d) {
		return path.Join(c.opt.Instance, "uploads", uuid.New().String(),
			"compressed-blobs",
			strings.ToLower(c.getCompressor().String()),
			d.Hash,
			strconv.FormatInt(d.SizeBytes, 10))
	}
	return path.Join(c.opt.Instance, "uploads", uuid.New().String(), "blobs", d.Hash, strconv.FormatInt(d.SizeBytes, 10))
}

// newEncoder returns an encoder to compress blob.
// For uncompressed blob, it returns a nop closer.
func (c *Client) newEncoder(w io.Writer, d digest.Digest) (io.WriteCloser, error) {
	if c.useCompressedBlob(d) {
		switch comp := c.getCompressor(); comp {
		case rpb.Compressor_ZSTD:
			return zstd.NewWriter(w)
		case rpb.Compressor_DEFLATE:
			return flate.NewWriter(w, flate.DefaultCompression)
		default:
			return nil, fmt.Errorf("unsupported compressor %q", comp)
		}
	}
	return nopWriteCloser{w}, nil
}

type nopWriteCloser struct {
	io.Writer
}

func (nopWriteCloser) Close() error { return nil }
