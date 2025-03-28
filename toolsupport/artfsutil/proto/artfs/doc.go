// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package artfs provides protocol buffer message for artfs.
package artfs

//go:generate ../../../../scripts/install-protoc-gen-go
//go:generate protoc -I. -I.. --go_out=. --go_opt=paths=source_relative --go-grpc_out=. --go-grpc_opt=paths=source_relative artfs.proto
