// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package reproxyexec executes cmd with reproxy.
package reproxyexec

// grpcMaxMsgSize is the max value of gRPC response that can be received by the client (in bytes).
const grpcMaxMsgSize = 1024 * 1024 * 32 // 32MB (default is 4MB)
