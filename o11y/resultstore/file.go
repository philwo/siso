// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package resultstore

import (
	"context"
	"fmt"

	rspb "google.golang.org/genproto/googleapis/devtools/resultstore/v2"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"infra/build/siso/reapi/digest"
)

// UploadFiles uploads files to RBE-CAS, and sets the files as the invocation's artifact.
// Need to set Dir, HashFS, REAPIClient to Uploader before calling this.
func (u *Uploader) UploadFiles(ctx context.Context, fnames []string) error {
	if u.HashFS == nil || u.REAPIClient == nil {
		return fmt.Errorf("resultstore: unable to upload file. hashfs or reapi client is not set")
	}
	ents, err := u.HashFS.Entries(ctx, u.Dir, fnames)
	if err != nil {
		return err
	}
	ds := digest.NewStore()
	var files []*rspb.File
	for _, ent := range ents {
		file := &rspb.File{
			Uid: ent.Name,
		}
		if ent.Data.IsZero() {
			continue
		}
		ds.Set(ent.Data)
		d := ent.Data.Digest()
		file.Uri = u.REAPIClient.FileURI(d)
		file.Length = &wrapperspb.Int64Value{
			Value: d.SizeBytes,
		}
		// file.ContentType ?
		file.Digest = d.Hash
		file.HashType = rspb.File_SHA256
		files = append(files, file)
	}
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case u.q <- ds:
	}
	req := &rspb.UploadRequest{
		UploadOperation: rspb.UploadRequest_MERGE,
		UpdateMask: &fieldmaskpb.FieldMask{
			Paths: []string{
				"files",
			},
		},
		Resource: &rspb.UploadRequest_Invocation{
			Invocation: &rspb.Invocation{
				Files: files,
			},
		},
	}
	return u.Upload(ctx, req)

}
