// Copyright 2023 The Chromium Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package hashfs provides a filesystem with digest hash.
package hashfs

import "infra/build/siso/reapi/digest"

// DataSource is an interface to get digest data for digest and its name.
type DataSource interface {
	DigestData(digest.Digest, string) digest.Data
}
