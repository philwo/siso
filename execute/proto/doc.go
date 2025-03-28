// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package proto provides protocol buffer message for execute.
package proto

//go:generate ../../scripts/install-protoc-gen-go
//go:generate protoc -I. --go_out=. --go_opt=paths=source_relative rusage.proto
