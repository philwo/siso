// Copyright 2023 The Chromium Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package digest

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"

	"github.com/bazelbuild/remote-apis-sdks/go/pkg/digest"
	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/protobuf/encoding/prototext"
)

var digestPattern = regexp.MustCompile(`^([0-9a-fA-F]{64})/([0-9]+)$`)

// Parse parses digest string representation.
// It accepts the following string formats.
//   - hash/size_bytes
//   - json representation of digest.
//   - proto text representation of digest.
//
// TODO(b/266518906): move this utility to somewhere else for subcmd/{recall, fetch, cache}.
func Parse(s string) (digest.Digest, error) {
	var d digest.Digest
	m := digestPattern.FindStringSubmatch(s)
	if len(m) == 3 {
		d.Hash = m[1]
		var err error
		d.Size, err = strconv.ParseInt(m[2], 10, 64)
		if err == nil {
			return d, nil
		}
	}
	err := json.Unmarshal([]byte(s), &d)
	if err == nil {
		return d, nil
	}
	msg := &rpb.Digest{}
	perr := prototext.Unmarshal([]byte(s), msg)
	if perr == nil {
		return digest.NewFromProto(msg)
	}
	return d, fmt.Errorf("failed to unmarshal %T json:%v proto:%v", msg, err, perr)
}
