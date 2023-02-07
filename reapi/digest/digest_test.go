// Copyright 2023 The Chromium Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package digest

import (
	"testing"
)

func TestDigest(t *testing.T) {
	// Regular case
	b := []byte{1, 2, 3}
	d := ofBytes(b)

	wantStr := "039058c6f2c0cb492c533b0a4d14ef77cc0f78abccced5287d84a1a2011cfb81/3"
	if d.String() != wantStr {
		t.Errorf("ofBytes(%v).String() = %s, want %s", b, d.String(), wantStr)
	}

	p := d.Proto()
	if p == nil {
		t.Errorf("ofBytes(%v).Proto() = nil, want a Digest proto", b)
	}

	dFromProto := FromProto(p)
	if dFromProto != d {
		t.Errorf("FromProto(%v) = %v, want %v", p, dFromProto, d)
	}

	// From nil proto
	nild := FromProto(nil)
	if nild.IsZero() != true {
		t.Errorf("FromProto(nil).IsZero() = false, want true")
	}

	// Empty digest
	empty := ofBytes([]byte{})
	if empty.SizeBytes != 0 {
		t.Errorf("ofBytes([]byte{}).SizeBytes = %v, want 0", empty.SizeBytes)
	}
	if empty.IsZero() {
		t.Errorf("ofBytes([]byte{}).IsZero() = true, want false")
	}

}
