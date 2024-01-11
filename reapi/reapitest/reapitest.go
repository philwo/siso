// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package reapitest provides fake implementation of reapi for test.
package reapitest

import (
	"context"
	"fmt"
	"net"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bazelbuild/remote-apis-sdks/go/pkg/digest"
	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"infra/build/kajiya/actioncache"
	"infra/build/kajiya/blobstore"
	"infra/build/kajiya/capabilities"
	"infra/build/kajiya/execution"
	"infra/build/siso/reapi"
)

// Fake is fake reapi server.
type Fake struct {
	CAS *blobstore.ContentAddressableStorage

	ExecuteFunc func(*Fake, *rpb.Action) (*rpb.ActionResult, error)
}

// Execute runs command on fake reapi.
func (f *Fake) Execute(action *rpb.Action) (*rpb.ActionResult, error) {
	if f.ExecuteFunc == nil {
		return nil, status.Error(codes.Unimplemented, "nil ExecuteFunc")
	}
	return f.ExecuteFunc(f, action)
}

type server struct {
	addr     string
	cleanups []func()
	closed   chan struct{}
}

func newServer(ctx context.Context, t *testing.T, fake *Fake) *server {
	t.Helper()
	s := &server{
		closed: make(chan struct{}),
	}
	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	s.cleanups = append(s.cleanups, func() {
		err := lis.Close()
		if err != nil {
			t.Error(err)
		}
	})
	s.addr = lis.Addr().String()
	t.Logf("fake reapi at %s", s.addr)

	dir := t.TempDir()
	serv := grpc.NewServer()
	capabilities.Register(serv)

	casDir := filepath.Join(dir, "cas")
	cas, err := blobstore.New(casDir)
	if err != nil {
		t.Fatal(err)
	}
	fake.CAS = cas

	uploadDir := filepath.Join(casDir, "tmp")
	err = blobstore.Register(serv, cas, uploadDir)
	if err != nil {
		t.Fatal(err)
	}
	acDir := filepath.Join(dir, "ac")
	ac, err := actioncache.New(acDir)
	if err != nil {
		t.Fatal(err)
	}
	err = actioncache.Register(serv, ac, cas)
	if err != nil {
		t.Fatal(err)
	}

	err = execution.Register(serv, fake, ac, cas)
	if err != nil {
		t.Fatal(err)
	}
	reflection.Register(serv)
	go func() {
		defer close(s.closed)
		err := serv.Serve(lis)
		t.Logf("Serve finished: %v", err)
	}()
	return s
}

func (s *server) Close() {
	for i := len(s.cleanups) - 1; i >= 0; i-- {
		s.cleanups[i]()
	}
	s.addr = ""
	s.cleanups = nil
	<-s.closed
}

// New starts new fake reapi grpc server and returns reapi client.
func New(ctx context.Context, t *testing.T, fake *Fake) *reapi.Client {
	t.Helper()
	s := newServer(ctx, t, fake)
	t.Cleanup(s.Close)
	opt := reapi.Option{
		Address:  s.addr,
		Instance: "projects/siso-test/instances/default_instance",
	}
	conn, err := grpc.DialContext(ctx, s.addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	client, err := reapi.NewFromConn(ctx, opt, conn)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

type InputTree struct {
	CAS  *blobstore.ContentAddressableStorage
	Root *rpb.Digest
}

func (t InputTree) get(ctx context.Context, d *rpb.Digest, m proto.Message) error {
	dd, err := digest.NewFromProto(d)
	if err != nil {
		return err
	}
	b, err := t.CAS.Get(dd)
	if err != nil {
		return err
	}
	return proto.Unmarshal(b, m)
}

// LookupFileNode looks up name's file node in tree.
func (t InputTree) LookupFileNode(ctx context.Context, name string) (*rpb.FileNode, error) {
	dir := &rpb.Directory{}
	err := t.get(ctx, t.Root, dir)
	if err != nil {
		return nil, err
	}
	var elems []string
pathElements:
	for _, elem := range strings.Split(path.Dir(name), "/") {
		for _, s := range dir.Directories {
			if elem == s.Name {
				subdir := &rpb.Directory{}
				err = t.get(ctx, s.Digest, subdir)
				if err != nil {
					return nil, fmt.Errorf("missing %s %s: %w", strings.Join(elems, "/"), s.Digest, err)
				}
				dir = subdir
				elems = append(elems, elem)
				continue pathElements
			}
		}
		return nil, fmt.Errorf("missing dir %s in %s: %s", elem, strings.Join(elems, "."), dir.Directories)
	}
	elem := path.Base(name)
	for _, f := range dir.Files {
		if elem == f.Name {
			return f, nil
		}
	}
	return nil, fmt.Errorf("missing file %s in %s: %s", elem, path.Dir(name), dir.Files)
}
