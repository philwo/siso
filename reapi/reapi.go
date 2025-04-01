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
	"io"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/api/option"
	gtransport "google.golang.org/api/transport/grpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	"go.chromium.org/luci/cipd/version"

	"go.chromium.org/infra/build/siso/auth/cred"
	"go.chromium.org/infra/build/siso/o11y/clog"
	"go.chromium.org/infra/build/siso/o11y/iometrics"
	"go.chromium.org/infra/build/siso/reapi/digest"
)

// Option contains options of remote exec API.
type Option struct {
	Prefix   string
	Address  string
	Instance string

	// Insecure mode for RE API.
	Insecure bool

	// use compressed blobs if server supports compressed blobs and size is bigger than this.
	// When 0 is set, blob compression is disabled.
	CompressedBlob int64

	KeepAliveParams keepalive.ClientParameters
}

// RegisterFlags registers flags on the option.
func (o *Option) RegisterFlags(fs *flag.FlagSet, envs map[string]string) {
	var purpose string
	if o.Prefix == "" {
		o.Prefix = "reapi"
	} else {
		purpose = fmt.Sprintf(" (for %s)", o.Prefix)
	}
	addr := envs["SISO_REAPI_ADDRESS"]
	if addr == "" {
		addr = "remotebuildexecution.googleapis.com:443"
	}
	fs.StringVar(&o.Address, o.Prefix+"_address", addr, "reapi address"+purpose)
	instance := envs["SISO_REAPI_INSTANCE"]
	if instance == "" {
		instance = "default_instance"
	}
	fs.StringVar(&o.Instance, o.Prefix+"_instance", instance, "reapi instance name"+purpose)

	fs.BoolVar(&o.Insecure, o.Prefix+"_insecure", os.Getenv("RBE_service_no_security") == "true", "reapi insecure mode")

	fs.Int64Var(&o.CompressedBlob, o.Prefix+"_compress_blob", 1024, "use compressed blobs if server supports compressed blobs and size is bigger than this. specify 0 to disable comporession."+purpose)

	// https://grpc.io/docs/guides/keepalive/#keepalive-configuration-specification
	// b/286237547 - RBE suggests 30s
	fs.DurationVar(&o.KeepAliveParams.Time, o.Prefix+"_grpc_keepalive_time", 30*time.Second, "grpc keepalive time"+purpose)
	fs.DurationVar(&o.KeepAliveParams.Timeout, o.Prefix+"_grpc_keepalive_timeout", 20*time.Second, "grpc keepalive timeout"+purpose)
	fs.BoolVar(&o.KeepAliveParams.PermitWithoutStream, o.Prefix+"_grpc_keepalive_permit_without_stream", false, "grpc keepalive permit without stream"+purpose)
}

// UpdateProjectID updates the Option for projID and returns cloud project ID to use.
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

type grpcClientConn interface {
	grpc.ClientConnInterface
	io.Closer
}

// Client is a remote exec API client.
type Client struct {
	opt  Option
	conn grpcClientConn

	capabilities *rpb.ServerCapabilities
	knownDigests sync.Map // key:digest.Digest, value: *uploadOp or true

	m *iometrics.IOMetrics
}

// serviceConfig is gRPC service config for RE API.
// https://github.com/bazelbuild/bazel/blob/7.1.1/src/main/java/com/google/devtools/build/lib/remote/RemoteRetrier.java#L47
var serviceConfig = `
{
	"loadBalancingConfig": [{"round_robin":{}}],
	"methodConfig": [
	  {
		"name": [
                  { "service": "build.bazel.remote.execution.v2.Execution" }
                ],
		"timeout": "600s",
		"retryPolicy": {
			"maxAttempts": 5,
			"initialBackoff": "1s",
			"maxBackoff": "120s",
			"backoffMultiplier": 1.6,
			"retryableStatusCodes": [
				"ABORTED",
				"INTERNAL",
				"RESOURCE_EXHAUSTED",
				"UNAVAILABLE",
				"UNKNOWN"
			]
		}
	  },
	  {
		"name": [
                  { "service": "build.bazel.remote.execution.v2.ContentAddressableStorage" },
                  { "service": "build.bazel.remote.execution.v2.Capabilities" }
                ],
		"timeout": "600s",
		"retryPolicy": {
			"maxAttempts": 5,
			"initialBackoff": "0.1s",
			"maxBackoff": "1s",
			"backoffMultiplier": 1.6,
			"retryableStatusCodes": [
				"ABORTED",
				"INTERNAL",
				"RESOURCE_EXHAUSTED",
				"UNAVAILABLE",
				"UNKNOWN"
			]
		}
	  }
	]
}`

func dialOptions(keepAliveParams keepalive.ClientParameters) []grpc.DialOption {
	// TODO(b/273639326): handle auth failures gracefully.

	// https://github.com/grpc/grpc/blob/c16338581dba2b054bf52484266b79e6934bbc1c/doc/service_config.md
	// https://github.com/grpc/proposal/blob/9f993b522267ed297fe54c9ee32cfc13699166c7/A6-client-retries.md
	// timeout=300s may cause deadline exceeded to fetch large *.so file?
	dopts := append([]grpc.DialOption(nil),
		grpc.WithKeepaliveParams(keepAliveParams),
		grpc.WithDisableServiceConfig(),
		// no retry for ActionCache
		grpc.WithDefaultServiceConfig(serviceConfig),
	)
	return dopts
}

// New creates new remote exec API client.
func New(ctx context.Context, cred cred.Cred, opt Option) (*Client, error) {
	if opt.Address == "" {
		return nil, errors.New("no reapi address")
	}
	if opt.Instance == "" {
		return nil, errors.New("no reapi instance")
	}
	clog.Infof(ctx, "address: %q instance: %q", opt.Address, opt.Instance)

	copts := []option.ClientOption{
		option.WithEndpoint(opt.Address),
		option.WithGRPCConnectionPool(25),
	}
	dopts := dialOptions(opt.KeepAliveParams)
	if opt.Insecure {
		if strings.HasSuffix(opt.Address, ".googleapis.com:443") {
			return nil, errors.New("insecure mode is not supported for RBE")
		}
		clog.Warningf(ctx, "insecure mode")
		copts = append(copts, option.WithoutAuthentication())
		dopts = append(dopts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	} else {
		copts = append(copts, cred.ClientOptions()...)
	}
	for _, dopt := range dopts {
		copts = append(copts, option.WithGRPCDialOption(dopt))
	}

	conn, err := gtransport.DialPool(ctx, copts...)
	if err != nil {
		return nil, fmt.Errorf("failed to dial %s: %w", opt.Address, err)
	}
	return NewFromConn(ctx, opt, conn)
}

// NewFromConn creates new remote exec API client from conn.
func NewFromConn(ctx context.Context, opt Option, conn grpcClientConn) (*Client, error) {
	cc := rpb.NewCapabilitiesClient(conn)
	capa, err := cc.GetCapabilities(ctx, &rpb.GetCapabilitiesRequest{
		InstanceName: opt.Instance,
	})
	if err != nil {
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
	c := &Client{
		opt:          opt,
		conn:         conn,
		capabilities: capa,
		m:            iometrics.New("reapi"),
	}
	c.knownDigests.Store(digest.Empty, true)
	return c, nil
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

// MetadataFromOutgoingContext returns request metadata in outgoing context.
func MetadataFromOutgoingContext(ctx context.Context) (*rpb.RequestMetadata, bool) {
	md, ok := metadata.FromOutgoingContext(ctx)
	if !ok {
		return nil, false
	}
	v, ok := md["build.bazel.remote.execution.v2.requestmetadata-bin"]
	if !ok {
		return nil, false
	}
	if len(v) == 0 {
		return nil, false
	}
	rmd := &rpb.RequestMetadata{}
	err := proto.Unmarshal([]byte(v[0]), rmd)
	if err != nil {
		return nil, false
	}
	return rmd, true
}
