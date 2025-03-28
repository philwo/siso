// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package osfs provides OS Filesystem access.
package osfs

import (
	"context"
	"fmt"
	"io"
	"os"
)

// FileSource creates new FileSource for name.
func NewFileSource(name string) FileSource {
	return FileSource{Fname: name}
}

// FileSource is a file source.
type FileSource struct {
	Fname string
}

// Open opens the named file for reading.
func (fsc FileSource) Open(ctx context.Context) (io.ReadCloser, error) {
	return os.Open(fsc.Fname)
}

func (fsc FileSource) String() string {
	return fmt.Sprintf("file://%s", fsc.Fname)
}
