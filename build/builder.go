// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

// logging labels's key.
const (
	logLabelKeyID        = "id"
	logLabelKeyBacktrace = "backtrace"
)

// Builder is a builder.
type Builder struct {
	stepDefs      StepDefs
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
