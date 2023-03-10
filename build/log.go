// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"fmt"

	"cloud.google.com/go/logging"
)

func logFormat(e logging.Entry) string {
	stepID := e.Labels["id"]
	if e.HTTPRequest != nil {
		return fmt.Sprintf("%s %v %s", stepID, e.Payload, e.HTTPRequest.Latency)
	}
	if stepID == "" {
		return fmt.Sprintf("%v", e.Payload)
	}
	return fmt.Sprintf("%s %v", stepID, e.Payload)
}
