// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.
// Package reapi provides remote execution API.
package reapi

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"path"
	"strings"

	"github.com/bazelbuild/remote-apis-sdks/go/pkg/balancer"
	configpb "github.com/bazelbuild/remote-apis-sdks/go/pkg/balancer/proto"
	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"go.chromium.org/luci/cipd/version"
	"golang.org/x/sync/singleflight"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	"infra/build/siso/auth/cred"
	"infra/build/siso/o11y/clog"
	"infra/build/siso/o11y/iometrics"
	"infra/build/siso/reapi/digest"
)

// Option contains options of remote exec API.
type Option struct {
	Address  string
	Instance string

	// use compressed blobs if server supports compressed blobs and size is bigger than this.
	// When 0 is set, blob compression is disabled.
	CompressedBlob int64
}

// RegisterFlags registers flags on the option.
func (o *Option) RegisterFlags(fs *flag.FlagSet, envs map[string]string) {
	addr := envs["SISO_REAPI_ADDRESS"]
	if addr == "" {
		addr = "remotebuildexecution.googleapis.com:443"
	}
	fs.StringVar(&o.Address, "reapi_address", addr, "reapi address")
	instance := envs["SISO_REAPI_INSTANCE"]
	if instance == "" {
		instance = "default_instance"
	}
	fs.StringVar(&o.Instance, "reapi_instance", instance, "reapi instance name")
	fs.Int64Var(&o.CompressedBlob, "reapi_compress_blob", 1024, "use compressed blobs if server supports compressed blobs and size is bigger than this. specify 0 to disable comporession.")
}

func (o *Option) UpdateProjectID(projID string) string {
	if projID != "" && !strings.HasPrefix(o.Instance, "projects/") {
		o.Instance = path.Join("projects", projID, "instances", o.Instance)
	}
	if projID == "" && strings.HasPrefix(o.Instance, "projects/") {
		projID = strings.Split(o.Instance, "/")[1]
	}
	if projID == "" && !strings.HasPrefix(o.Instance, "projects/") {
		// make Option invalid.
		o.Instance = ""
	}
	return projID
}

// IsValid returns whether option is valid or not.
func (o Option) IsValid() bool {
	return o.Address != "" && o.Instance != ""
}

// Client is a remote exec API client.
type Client struct {
	opt  Option
	conn *grpc.ClientConn

	capabilities           *rpb.ServerCapabilities
	bytestreamSingleflight singleflight.Group

	m *iometrics.IOMetrics
}

// New creates new remote exec API client.
func New(ctx context.Context, cred cred.Cred, opt Option) (*Client, error) {
	if opt.Address == "" {
		return nil, errors.New("no reapi address")
	}
	if opt.Instance == "" {
		return nil, errors.New("no reapi isntance")
	}
	clog.Infof(ctx, "address: %q instance: %q", opt.Address, opt.Instance)

	// TODO(b/273639326): handle auth failures gracefully.

	// github.com/bazelbuild/remote-apis-sdks/go/pkg/balancer
	// We use this primarily to create new sub-connections when we reach
	// maximum number of streams (100 for GFE) on a given connection.
	// Refer to https://github.com/grpc/grpc/issues/21386 for status on the long-term fix
	// for this issue.
	apiConfig := &configpb.ApiConfig{
		ChannelPool: &configpb.ChannelPoolConfig{
			MaxSize:                          25,
			MaxConcurrentStreamsLowWatermark: 75,
		},
		Method: []*configpb.MethodConfig{
			{
				Name: []string{".*"},
				Affinity: &configpb.AffinityConfig{
					Command:     configpb.AffinityConfig_BIND,
					AffinityKey: "bind-affinity",
				},
			},
		},
	}
	grpcInt := balancer.NewGCPInterceptor(apiConfig)

	// https://github.com/grpc/grpc/blob/c16338581dba2b054bf52484266b79e6934bbc1c/doc/service_config.md
	// https://github.com/grpc/proposal/blob/9f993b522267ed297fe54c9ee32cfc13699166c7/A6-client-retries.md
	// timeout=300s may cause deadline exceeded to fetch large *.so file?
	dopts := append(cred.GRPCDialOptions(),
		grpc.WithUnaryInterceptor(grpcInt.GCPUnaryClientInterceptor),
		grpc.WithStreamInterceptor(grpcInt.GCPStreamClientInterceptor),
		grpc.WithDisableServiceConfig(),
		grpc.WithDefaultServiceConfig(fmt.Sprintf(`
{
	"loadBalancingConfig": [{%q:{}}],
	"methodConfig": [
	  {
		"name": [ { } ],
		"timeout": "600s",
		"retryPolicy": {
			"maxAttempts": 5,
			"initialBackoff": "1s",
			"maxBackoff": "120s",
			"backoffMultiplier": 1.6,
			"retriableStatusCodes": [
				"RESOURCE_EXHAUSTED",
				"UNAVAILABLE"
			]
		}
	  }
	]
}`, balancer.Name)))

	conn, err := grpc.DialContext(ctx, opt.Address, dopts...)
	if err != nil {
		return nil, fmt.Errorf("failed to dial %s: %w", opt.Address, err)
	}
	cc := rpb.NewCapabilitiesClient(conn)
	capa, err := cc.GetCapabilities(ctx, &rpb.GetCapabilitiesRequest{
		InstanceName: opt.Instance,
	})
	if err != nil {
		clog.Errorf(ctx, "Failed to get capabilities: %v", err)
		conn.Close()
		return nil, err
	}
	clog.Infof(ctx, "capabilities of %s: %s", opt.Instance, capa)
	if opt.CompressedBlob > 0 {
		if len(capa.GetCacheCapabilities().SupportedCompressors) > 0 && capa.CacheCapabilities.SupportedCompressors[0] != rpb.Compressor_IDENTITY {
			clog.Infof(ctx, "compressed-blobs/%s for > %d", strings.ToLower(capa.CacheCapabilities.SupportedCompressors[0].String()), opt.CompressedBlob)
		} else {
			clog.Infof(ctx, "compressed-blobs is not supported")
			opt.CompressedBlob = 0
		}
	}
	return &Client{
		opt:          opt,
		conn:         conn,
		capabilities: capa,
		m:            iometrics.New("reapi"),
	}, nil
}

// Close closes the client.
func (c *Client) Close() error {
	return c.conn.Close()
}

// IOMetrics returns an IOMetrics of the client.
func (c *Client) IOMetrics() *iometrics.IOMetrics {
	if c == nil {
		return nil
	}
	return c.m
}

// Proto fetches contents of digest into proto message.
func (c *Client) Proto(ctx context.Context, d digest.Digest, p proto.Message) error {
	b, err := c.Get(ctx, d, fmt.Sprintf("%s -> %T", d, p))
	if err != nil {
		return err
	}
	return proto.Unmarshal(b, p)
}

// GetActionResult gets the action result by the digest.
func (c *Client) GetActionResult(ctx context.Context, d digest.Digest) (*rpb.ActionResult, error) {
	client := rpb.NewActionCacheClient(c.conn)
	result, err := client.GetActionResult(ctx, &rpb.GetActionResultRequest{
		InstanceName: c.opt.Instance,
		ActionDigest: d.Proto(),
	})
	c.m.OpsDone(err)
	return result, err
}

// NewContext returns new context with request metadata.
func NewContext(ctx context.Context, rmd *rpb.RequestMetadata) context.Context {
	ver, err := version.GetStartupVersion()
	if err == nil {
		rmd.ToolDetails = &rpb.ToolDetails{
			ToolName:    ver.PackageName,
			ToolVersion: ver.InstanceID,
		}
	}
	// Append metadata to the context.
	// See the document for the specification.
	// https://github.com/bazelbuild/remote-apis/blob/8f539af4b407a4f649707f9632fc2b715c9aa065/build/bazel/remote/execution/v2/remote_execution.proto#L2034-L2045
	b, err := proto.Marshal(rmd)
	if err != nil {
		clog.Warningf(ctx, "marshal %v: %v", rmd, err)
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx,
		"build.bazel.remote.execution.v2.requestmetadata-bin",
		string(b))
}
