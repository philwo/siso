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
	"time"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/charmbracelet/log"
	"github.com/google/uuid"
	"github.com/klauspost/compress/zstd"
	"golang.org/x/sync/errgroup"
	bpb "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"go.chromium.org/infra/build/siso/reapi/bytestreamio"
	"go.chromium.org/infra/build/siso/reapi/digest"
)

const (
	// bytestreamReadThreshold is the threshold that decides whether to use BatchReadBlobs or ByteStream API.
	bytestreamReadThreshold = 2 * 1024 * 1024

	// defaultBatchUpdateByteLimit is bytes limit for cas BatchUpdateBlobs.
	defaultBatchUpdateByteLimit = 4 * 1024 * 1024

	// batchBlobUploadLimit is max number of blobs in BatchUpdateBlobs.
	batchBlobUploadLimit = 1000

	bytestreamSlowThroughputPerSec = 1 * 1024 * 1024
)

type uploadOp struct {
	ch  chan struct{}
	err error
}

func newUploadOp() *uploadOp {
	return &uploadOp{
		ch:  make(chan struct{}),
		err: errUploadNotFinished,
	}
}

func (u *uploadOp) done(err error) {
	u.err = err
	close(u.ch)
}

func (u *uploadOp) wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-u.ch:
		return u.err
	}
}

var errUploadNotFinished = errors.New("upload not finished")

func contextWithTimeoutForBytestream(ctx context.Context, d digest.Digest) (context.Context, context.CancelFunc) {
	timeout := max(time.Duration(d.SizeBytes/bytestreamSlowThroughputPerSec)*time.Second, 10*time.Minute)
	if timeout > 10*time.Minute {
		log.Infof("bytestream timeout for %s: %s", d, timeout)
	}
	return context.WithTimeout(ctx, timeout)
}

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

	if d.SizeBytes < bytestreamReadThreshold {
		return c.getWithBatchReadBlobs(ctx, d, name)
	}
	return c.getWithByteStream(ctx, d)
}

// getWithBatchReadBlobs fetches the content of blob using BatchReadBlobs rpc of CAS.
func (c *Client) getWithBatchReadBlobs(ctx context.Context, d digest.Digest, name string) ([]byte, error) {
	started := time.Now()
	casClient := rpb.NewContentAddressableStorageClient(c.casConn)
	resp, err := casClient.BatchReadBlobs(ctx, &rpb.BatchReadBlobsRequest{
		InstanceName: c.opt.Instance,
		Digests:      []*rpb.Digest{d.Proto()},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to read blobs %s for %s in %s: %w", d, name, time.Since(started), err)
	}
	if len(resp.Responses) != 1 {
		return nil, fmt.Errorf("failed to read blobs %s for %s in %s: responses=%d", d, name, time.Since(started), len(resp.Responses))
	}
	if int64(len(resp.Responses[0].Data)) != d.SizeBytes {
		return nil, fmt.Errorf("failed to read blobs %s for %s in %s: size mismatch got=%d", d, name, time.Since(started), len(resp.Responses[0].Data))
	}
	return resp.Responses[0].Data, nil
}

// getWithByteStream fetches the content of blob using the ByteStream API
func (c *Client) getWithByteStream(ctx context.Context, d digest.Digest) ([]byte, error) {
	ctx, cancel := contextWithTimeoutForBytestream(ctx, d)
	defer cancel()

	r, err := bytestreamio.Open(ctx, bpb.NewByteStreamClient(c.casConn), c.resourceName(d))
	if err != nil {
		return nil, err
	}

	rd, err := c.newDecoder(r, d)
	if err != nil {
		return nil, err
	}
	defer rd.Close()

	buf := make([]byte, d.SizeBytes)
	_, err = io.ReadFull(rd, buf)
	if err != nil {
		return nil, err
	}

	return buf, nil
}

// Missing returns digests of missing blobs.
func (c *Client) Missing(ctx context.Context, blobs []digest.Digest) ([]digest.Digest, error) {
	blobspb := make([]*rpb.Digest, 0, len(blobs))
	for _, b := range blobs {
		blobspb = append(blobspb, b.Proto())
	}

	cas := rpb.NewContentAddressableStorageClient(c.casConn)
	resp, err := cas.FindMissingBlobs(ctx, &rpb.FindMissingBlobsRequest{
		InstanceName: c.opt.Instance,
		BlobDigests:  blobspb,
	})
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
func (c *Client) UploadAll(ctx context.Context, ds *digest.Store) (numUploaded int, err error) {
	if c.casConn == nil {
		return 0, status.Error(codes.FailedPrecondition, "conn is not configured")
	}

	// First, partition the "blobs" list into three possible cases.
	blobs := ds.List()
	newBlobs := make(map[digest.Digest]*uploadOp)
	pendingBlobs := make(map[digest.Digest]*uploadOp)
	skippedBlobs := 0
	for _, d := range blobs {
		uop, loaded := c.knownDigests.LoadOrStore(d, newUploadOp())
		if loaded {
			switch v := uop.(type) {
			case bool:
				// Case 1: This blob is already present in the CAS, we're done.
				skippedBlobs++
			case *uploadOp:
				// Case 2: We need to wait for another thread to upload this blob.
				pendingBlobs[d] = v
			default:
				panic(fmt.Sprintf("unknown type %T", v))
			}
			continue
		}
		// Case 3: We need to upload this blob after confirming that it's missing.
		newBlobs[d] = uop.(*uploadOp)
	}

	defer func() {
		for d, uop := range newBlobs {
			switch uop.err {
			case nil:
				c.knownDigests.CompareAndSwap(d, uop, true)
			case errUploadNotFinished:
				close(uop.ch)
			default:
				log.Infof("upload %s failed: %v", d, uop.err)
				c.knownDigests.CompareAndDelete(d, uop)
			}
		}
	}()

	// For all "new" blobs, use FindMissingBlobs to ask the remote CAS which of them are
	// really still missing - they might already be present and we just don't know about it yet.
	var missingBlobs []digest.Digest
	var foundBlobs map[digest.Digest]bool
	var durFindMissing time.Duration
	if len(newBlobs) > 0 {
		t := time.Now()
		newBlobDigests := make([]digest.Digest, 0, len(newBlobs))
		foundBlobs = make(map[digest.Digest]bool, len(newBlobs))
		for d := range newBlobs {
			newBlobDigests = append(newBlobDigests, d)
			foundBlobs[d] = true
		}
		missingBlobs, err = c.Missing(ctx, newBlobDigests)
		if err != nil {
			for _, v := range newBlobs {
				v.done(err)
			}
			return 0, err
		}
		// All "new" blobs that the remote CAS did *not* report as missing are by inverse confirmed
		// to be present, so we can memoize this in the knownDigests map and notify any other
		// threads waiting for them.
		for _, d := range missingBlobs {
			delete(foundBlobs, d)
		}
		for d := range foundBlobs {
			c.knownDigests.CompareAndSwap(d, newBlobs[d], true)
			newBlobs[d].done(nil)
			delete(newBlobs, d)
		}
		durFindMissing = time.Since(t)
	}

	// Let's upload the blobs that we know are still missing.
	var durUpload time.Duration
	if len(missingBlobs) > 0 {
		t := time.Now()
		numUploaded, err = c.upload(ctx, ds, missingBlobs, newBlobs)
		if err != nil {
			return numUploaded, fmt.Errorf("upload: %w", err)
		}
		durUpload = time.Since(t)
	}

	// Finally, wait for any blobs that are being uploaded by other threads, before we return.
	var durWaitPending time.Duration
	if len(pendingBlobs) > 0 {
		t := time.Now()
		for d, uop := range pendingBlobs {
			if err := uop.wait(ctx); err != nil {
				return numUploaded, fmt.Errorf("wait for digest=%s: %w", d, err)
			}
		}
		durWaitPending = time.Since(t)
	}

	log.Infof("upload all: blobs=%d -> {uploaded=%d, found=%d, pending=%d, skipped=%d}, timing: {find_missing=%s, upload=%s, wait_pending=%s}",
		len(blobs), len(newBlobs), len(foundBlobs), len(pendingBlobs), skippedBlobs,
		durFindMissing.Round(time.Microsecond),
		durUpload.Round(time.Microsecond),
		durWaitPending.Round(time.Microsecond))

	return numUploaded, err
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

// upload uploads blobs in digest stores.
func (c *Client) upload(ctx context.Context, ds *digest.Store, blobs []digest.Digest, uploads map[digest.Digest]*uploadOp) (int, error) {
	byteLimit := int64(defaultBatchUpdateByteLimit)
	if max := c.capabilities.GetCacheCapabilities().GetMaxBatchTotalSizeBytes(); max > 0 {
		byteLimit = max
	}

	// Separate small blobs and large blobs because they are going to use different RPCs.
	smalls, larges := separateBlobs(c.opt.Instance, blobs, byteLimit)
	log.Infof("upload by batch %d out of %d", len(smalls), len(blobs))

	// Upload small blobs with BatchUpdateBlobs rpc.
	var missing missingError
	if len(smalls) > 0 {
		err := c.uploadWithBatchUpdateBlobs(ctx, smalls, uploads, ds, byteLimit)
		if err != nil {
			return 0, err
		}
	}

	// Upload large blobs with ByteStream API.
	if len(larges) > 0 {
		err := c.uploadAllWithByteStream(ctx, larges, uploads, ds)
		if err != nil {
			return 0, nil
		}
	}

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
func (c *Client) uploadWithBatchUpdateBlobs(ctx context.Context, digests []digest.Digest, uploads map[digest.Digest]*uploadOp, ds *digest.Store, byteLimit int64) error {
	blobReqs, err := lookupBlobsInStore(ctx, digests, ds)
	if err != nil {
		return err
	}

	// Bundle the blobs to multiple batch requests.
	batchReqs := createBatchUpdateBlobsRequests(c.opt.Instance, blobReqs, byteLimit)
	casClient := rpb.NewContentAddressableStorageClient(c.casConn)

	// TODO(b/273884978): It may be worth trying to send the batch requests in parallel.
	for _, batchReq := range batchReqs {
		n := 0
		for _, r := range batchReq.Requests {
			n += int(r.Digest.SizeBytes)
		}

		batchResp, err := casClient.BatchUpdateBlobs(ctx, batchReq)
		if err != nil {
			return status.Errorf(status.Code(err), "batch update blobs: %v", err)
		}

		var eg errgroup.Group
		for _, res := range batchResp.Responses {
			eg.Go(func() error {
				blob := digest.FromProto(res.Digest)
				data, ok := ds.Get(blob)
				if !ok {
					return fmt.Errorf("blob %s not found in store", blob)
				}

				st := status.FromProto(res.GetStatus())
				if st.Code() != codes.OK {
					err := status.Errorf(st.Code(), "batch update blobs: %v", res.Status)
					uploads[blob].done(err)
					return err
				}

				log.Infof("uploaded in batch: %s", data)
				uploads[blob].done(nil)
				return nil
			})
		}
		err = eg.Wait()
		if err != nil {
			return err
		}
		log.Infof("upload %d blobs by batch", len(batchReq.Requests))
	}
	return nil
}

func lookupBlobsInStore(ctx context.Context, blobs []digest.Digest, ds *digest.Store) ([]*rpb.BatchUpdateBlobsRequest_Request, error) {
	eg, ctx := errgroup.WithContext(ctx)
	results := make([]*rpb.BatchUpdateBlobsRequest_Request, len(blobs))
	for i, blob := range blobs {
		eg.Go(func() error {
			data, ok := ds.Get(blob)
			if !ok {
				return errBlobNotInReq
			}
			b, err := readAll(ctx, data)
			if err != nil {
				return err
			}
			results[i] = &rpb.BatchUpdateBlobsRequest_Request{
				Digest: data.Digest().Proto(),
				Data:   b,
			}
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return nil, err
	}
	return results, nil
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
	r, err := data.Open(ctx)
	if err != nil {
		return nil, err
	}
	defer r.Close()

	buf, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}

	return buf, nil
}

func (c *Client) uploadWithByteStream(ctx context.Context, bs bpb.ByteStreamClient, d digest.Data) error {
	ctx, cancel := contextWithTimeoutForBytestream(ctx, d.Digest())
	defer cancel()

	rd, err := d.Open(ctx)
	if err != nil {
		return err
	}
	defer rd.Close()
	wr, err := bytestreamio.Create(ctx, bs, c.uploadResourceName(d.Digest()))
	if err != nil {
		return err
	}
	cwr, err := c.newEncoder(wr, d.Digest())
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
}

func (c *Client) uploadAllWithByteStream(ctx context.Context, digests []digest.Digest, uploads map[digest.Digest]*uploadOp, ds *digest.Store) error {
	log.Infof("uploading %d blobs by streaming", len(digests))

	bsClient := bpb.NewByteStreamClient(c.casConn)
	eg, ctx := errgroup.WithContext(ctx)
	for _, d := range digests {
		eg.Go(func() (err error) {
			data, ok := ds.Get(d)
			if !ok {
				err = fmt.Errorf("blob %s not found in store", d)
			} else {
				err = c.uploadWithByteStream(ctx, bsClient, data)
			}
			uploads[d].done(err)
			if err != nil {
				return fmt.Errorf("failed to upload blob %s: %v", d, err)
			}
			return nil
		})
	}
	return eg.Wait()
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

// FileURI returns bytestream URI for digest.
func (c *Client) FileURI(d digest.Digest) string {
	// compressed-blobs is not supported?
	return fmt.Sprintf("bytestream://%s/%s", c.opt.Address, path.Join(c.opt.Instance, "blobs", d.Hash, strconv.FormatInt(d.SizeBytes, 10)))
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
