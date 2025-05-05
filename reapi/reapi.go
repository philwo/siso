// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package reapi provides remote execution API.
package reapi

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"sync"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/charmbracelet/log"
	"google.golang.org/api/option"
	gtransport "google.golang.org/api/transport/grpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	"go.chromium.org/infra/build/siso/auth/cred"
	"go.chromium.org/infra/build/siso/reapi/digest"
)

// Option contains options of remote exec API.
type Option struct {
	Prefix     string
	Address    string
	CASAddress string
	Instance   string

	// Insecure mode for RE API.
	Insecure bool

	// mTLS
	TLSClientAuthCert string
	TLSClientAuthKey  string

	// use compressed blobs if server supports compressed blobs and size is bigger than this.
	// When 0 is set, blob compression is disabled.
	CompressedBlob int64
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

// NeedCred returns whether credential is needed or not.
func (o Option) NeedCred() bool {
	if o.Insecure {
		return false
	}
	if o.TLSClientAuthCert != "" || o.TLSClientAuthKey != "" {
		return false
	}
	return true
}

type grpcClientConn interface {
	grpc.ClientConnInterface
	io.Closer
}

// Client is a remote exec API client.
type Client struct {
	opt     Option
	conn    grpcClientConn
	casConn grpcClientConn

	capabilities *rpb.ServerCapabilities
	knownDigests sync.Map // key:digest.Digest, value: *uploadOp or true
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

func dialOptions() []grpc.DialOption {
	// TODO(b/273639326): handle auth failures gracefully.

	// https://github.com/grpc/grpc/blob/c16338581dba2b054bf52484266b79e6934bbc1c/doc/service_config.md
	// https://github.com/grpc/proposal/blob/9f993b522267ed297fe54c9ee32cfc13699166c7/A6-client-retries.md
	// timeout=300s may cause deadline exceeded to fetch large *.so file?
	dopts := append([]grpc.DialOption(nil),
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
	log.Infof("address: %q instance: %q", opt.Address, opt.Instance)
	conn, err := newConn(ctx, opt.Address, cred, opt)
	if err != nil {
		return nil, err
	}
	casConn := conn
	if opt.CASAddress != "" {
		log.Infof("cas address: %q", opt.CASAddress)
		casConn, err = newConn(ctx, opt.CASAddress, cred, opt)
		if err != nil {
			conn.Close()
			return nil, err
		}
	}
	return NewFromConn(ctx, opt, conn, casConn)
}

func newConn(ctx context.Context, addr string, cred cred.Cred, opt Option) (grpcClientConn, error) {
	copts := []option.ClientOption{
		option.WithEndpoint(addr),
		option.WithGRPCConnectionPool(25),
	}
	dopts := dialOptions()
	var conn grpcClientConn
	var err error
	if opt.Insecure {
		if strings.HasSuffix(addr, ".googleapis.com:443") {
			return nil, errors.New("insecure mode is not supported for RBE")
		}
		log.Warnf("insecure mode")
		copts = append(copts, option.WithoutAuthentication())
		dopts = append(dopts, grpc.WithTransportCredentials(insecure.NewCredentials()))
		for _, dopt := range dopts {
			copts = append(copts, option.WithGRPCDialOption(dopt))
		}
		conn, err = gtransport.DialInsecure(ctx, copts...)
		if err != nil {
			return nil, fmt.Errorf("failed to dial %s: %w", addr, err)
		}
	} else if opt.TLSClientAuthCert != "" && opt.TLSClientAuthKey != "" {
		copts = append(copts, cred.ClientOptions()...)

		log.Infof("using mTLS: cert=%q key=%q", opt.TLSClientAuthCert, opt.TLSClientAuthKey)
		cert, err := tls.LoadX509KeyPair(opt.TLSClientAuthCert, opt.TLSClientAuthKey)
		if err != nil {
			return nil, fmt.Errorf("failed to read mTLS cert pair (%q, %q): %w", opt.TLSClientAuthCert, opt.TLSClientAuthKey, err)
		}
		copts = append(copts, option.WithClientCertSource(func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return &cert, nil
		}))
		for _, dopt := range dopts {
			copts = append(copts, option.WithGRPCDialOption(dopt))
		}
		conn, err = gtransport.Dial(ctx, copts...)
		if err != nil {
			return nil, fmt.Errorf("failed to dial %s: %w", addr, err)
		}
	} else if opt.TLSClientAuthCert != "" {
		return nil, errors.New("tls_client_auth_cert is set, but tls_client_auth_key is not set")
	} else if opt.TLSClientAuthKey != "" {
		return nil, errors.New("tls_client_auth_key is set, but tls_client_auth_cert is not set")
	} else {
		copts = append(copts, cred.ClientOptions()...)
		for _, dopt := range dopts {
			copts = append(copts, option.WithGRPCDialOption(dopt))
		}
		conn, err = gtransport.DialPool(ctx, copts...)
		if err != nil {
			return nil, fmt.Errorf("failed to dial %s: %w", addr, err)
		}
	}
	return conn, err
}

// NewFromConn creates new remote exec API client from conn and casConn.
func NewFromConn(ctx context.Context, opt Option, conn, casConn grpcClientConn) (*Client, error) {
	cc := rpb.NewCapabilitiesClient(conn)
	capa, err := cc.GetCapabilities(ctx, &rpb.GetCapabilitiesRequest{
		InstanceName: opt.Instance,
	})
	if err != nil {
		conn.Close()
		return nil, err
	}
	log.Infof("capabilities of %s: %s", opt.Instance, capa)
	if opt.CompressedBlob > 0 {
		if len(capa.GetCacheCapabilities().SupportedCompressors) > 0 && capa.CacheCapabilities.SupportedCompressors[0] != rpb.Compressor_IDENTITY {
			log.Infof("compressed-blobs/%s for > %d", strings.ToLower(capa.CacheCapabilities.SupportedCompressors[0].String()), opt.CompressedBlob)
		} else {
			log.Infof("compressed-blobs is not supported")
			opt.CompressedBlob = 0
		}
	}
	c := &Client{
		opt:          opt,
		conn:         conn,
		casConn:      casConn,
		capabilities: capa,
	}
	c.knownDigests.Store(digest.Empty, true)
	return c, nil
}

// Close closes the client.
func (c *Client) Close() error {
	return c.conn.Close()
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
	return result, err
}

// NewContext returns new context with request metadata.
func NewContext(ctx context.Context, rmd *rpb.RequestMetadata) context.Context {
	// Append metadata to the context.
	// See the document for the specification.
	// https://github.com/bazelbuild/remote-apis/blob/8f539af4b407a4f649707f9632fc2b715c9aa065/build/bazel/remote/execution/v2/remote_execution.proto#L2034-L2045
	b, err := proto.Marshal(rmd)
	if err != nil {
		log.Warnf("marshal %v: %v", rmd, err)
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx,
		"build.bazel.remote.execution.v2.requestmetadata-bin",
		string(b))
}
