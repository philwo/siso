// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package cogutil

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"

	"infra/build/siso/o11y/clog"
	"infra/build/siso/reapi"
	"infra/build/siso/reapi/digest"
	"infra/build/siso/reapi/merkletree"
	pb "infra/build/siso/toolsupport/cogutil/proto"
)

// Client is cogfs client.
type Client struct {
	reopt  *reapi.Option
	conn   *grpc.ClientConn
	client pb.CogLocalRpcServiceClient
}

// New creates new cog fs client at dir with reopt.
func New(ctx context.Context, dir string, reopt *reapi.Option) (*Client, error) {
	if !strings.HasPrefix(dir, "/google/cog/") {
		return nil, errors.ErrUnsupported
	}

	buf, err := os.ReadFile("/google/cog/status/version")
	if err != nil {
		return nil, err
	}
	clog.Infof(ctx, "cog version:\n%s", string(buf))
	if !reopt.IsValid() {
		clog.Warningf(ctx, "cog: reapi is not enabled")
		return &Client{}, nil
	}
	addr := fmt.Sprintf("unix:///google/cog/status/uds/%d", os.Getuid())
	conn, err := grpc.DialContext(ctx, addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		clog.Warningf(ctx, "cog: failed to dial to cog server %s: %v", addr, err)
		return &Client{}, nil
	}
	clog.Infof(ctx, "cog connected to %s", addr)
	c := &Client{
		reopt:  reopt,
		conn:   conn,
		client: pb.NewCogLocalRpcServiceClient(conn),
	}
	err = c.BuildfsInsert(ctx, dir, []merkletree.Entry{
		{
			Name: "out/.siso_cog_buildfs",
			Data: digest.FromBytes(".siso_cog_buildfs", nil),
		},
	})
	if err != nil {
		clog.Warningf(ctx, "cog: failed to insert .siso_cog_buildfs: %v", err)
		err = c.Close()
		if err != nil {
			clog.Warningf(ctx, "cog: close conn: %v", err)
		}
		// disable buildfs
		return &Client{}, nil
	}
	clog.Infof(ctx, "cog: buildfs available")
	return c, nil
}

// Info returns cog supported status.
func (c *Client) Info() string {
	if c == nil {
		return "cog disabled"
	}
	if c.conn == nil {
		return "cog enabled: buildfs disabled"
	}
	return "cog enabled: buildfs enabled"
}

// Close closes connection to cogfs server.
func (c *Client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	conn := c.conn
	c.conn = nil
	return conn.Close()
}

// BuildfsInsert inserts entries at dir.
func (c *Client) BuildfsInsert(ctx context.Context, dir string, entries []merkletree.Entry) error {
	if c == nil || c.conn == nil || c.client == nil {
		return errors.ErrUnsupported
	}
	addr := c.reopt.Address
	if !strings.HasPrefix(addr, "dns://") {
		addr = "dns:///" + addr
	}
	req := &pb.BuildfsInsertRequest{
		ReapiServer:   proto.String(addr),
		ReapiInstance: proto.String(c.reopt.Instance),
	}
	for _, entry := range entries {
		d := entry.Data.Digest()
		ins := &pb.BuildfsInsertion{
			Path:   proto.String(filepath.Join(dir, entry.Name)),
			Digest: proto.String(d.Hash),
			Size:   proto.Int64(d.SizeBytes),
		}
		if entry.IsExecutable {
			ins.Mode = pb.BuildfsInsertion_EXECUTABLE_FILE.Enum()
		}
		req.Insertions = append(req.Insertions, ins)
	}
	_, err := c.client.BuildfsInsert(ctx, req)
	clog.Infof(ctx, "buildfs insert %s: %v", req, err)
	return err
}
