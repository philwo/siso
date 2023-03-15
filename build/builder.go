// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"infra/build/siso/hashfs"
)

// logging labels's key.
const (
	logLabelKeyID        = "id"
	logLabelKeyBacktrace = "backtrace"
)

var experiments Experiments

// Builder is a builder.
type Builder struct {
	// path system used in the build.
	path   *Path
	hashFS *hashfs.HashFS

	// arg table to intern command line args of steps.
	argTab symtab

	stepDefs StepDefs

	reCacheEnableRead bool
	// TODO(b/266518906): enable reCacheEnableWrite option for read-only client.
	// reCacheEnableWrite bool

	actionSalt []byte

	sharedDepsLog SharedDepsLog
}

// Metadata is a metadata of the build process.
type Metadata struct {
	// KV is an arbitrary key-value pair in the metadata.
	KV     map[string]string
	NumCPU int
	GOOS   string
	GOARCH string
}

// TODO(b/266518906): enable envfile support when `ninja -t msvc -e envfile` is used.
// func (b *Builder) envfile(ctx context.Context, fname string) []string
