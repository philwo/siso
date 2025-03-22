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

	"github.com/charmbracelet/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"

	"go.chromium.org/infra/build/siso/reapi"
	"go.chromium.org/infra/build/siso/reapi/digest"
	"go.chromium.org/infra/build/siso/reapi/merkletree"
	pb "go.chromium.org/infra/build/siso/toolsupport/cogutil/proto"
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
	log.Infof("cog version:\n%s", string(buf))
	if !reopt.IsValid() {
		log.Warnf("cog: reapi is not enabled")
		return &Client{}, nil
	}
	addr := fmt.Sprintf("unix:///google/cog/status/uds/%d", os.Getuid())
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Warnf("cog: failed to dial to cog server %s: %v", addr, err)
		return &Client{}, nil
	}
	log.Infof("cog connected to %s", addr)
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
		log.Warnf("cog: failed to insert .siso_cog_buildfs: %v", err)
		err = c.Close()
		if err != nil {
			log.Warnf("cog: close conn: %v", err)
		}
		// disable buildfs
		return &Client{}, nil
	}
	log.Infof("cog: buildfs available")
	return c, nil
}

// Info returns cog supported status.
func (c *Client) Info() string {
	if c == nil {
		return "cog disabled"
	}
	if c.conn == nil {
		return "cog enabled"
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
	log.Infof("buildfs insert %s: %v", req, err)
	return err
}
