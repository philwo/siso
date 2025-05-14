// Copyright 2025 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package ninjalog contains structs relevant to siso in the ninjalog.
// This package is intentionally standalone so that it can be imported
// without pulling along the rest of siso.
package ninjalog

// SisoMetadata contains metadata that is populated directly by siso.
type SisoMetadata struct {
	// SisoVersion is the SemVer of siso.
	SisoVersion string `json:"siso_version"`
}
