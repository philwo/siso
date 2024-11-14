// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package hashfs

import (
	"context"
	"io/fs"
	"os"

	pb "infra/build/siso/hashfs/proto"
)

// FSMonitor provides a way to access file info on the disk for the entry.
type FSMonitor interface {
	// FileInfo returns file info for the entry.
	FileInfo(context.Context, *pb.Entry) (fs.FileInfo, error)
}

type fsScanner interface {
	// Scan scans fs info for FileInfo.
	Scan(context.Context) error
}

type osfsMonitor struct{}

// FileInfo returns file info of the entry.
func (osfsMonitor) FileInfo(ctx context.Context, ent *pb.Entry) (fs.FileInfo, error) {
	return os.Lstat(ent.Name)
}
