// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package reproxyexec executes cmd with reproxy.
package reproxyexec

import (
	"context"
	"fmt"

	"infra/build/siso/execute"
)

// grpcMaxMsgSize is the max value of gRPC response that can be received by the client (in bytes).
const grpcMaxMsgSize = 1024 * 1024 * 32 // 32MB (default is 4MB)

// ReproxyExec is executor with reproxy.
type ReproxyExec struct {
	serverAddr string
}

// New creates new remote executor.
func New(ctx context.Context, serverAddr string) *ReproxyExec {
	return &ReproxyExec{
		serverAddr: serverAddr,
	}
}

// Run runs a cmd.
func (re *ReproxyExec) Run(ctx context.Context, cmd *execute.Cmd) error {
	return fmt.Errorf("reproxyexec not implemented")
}
