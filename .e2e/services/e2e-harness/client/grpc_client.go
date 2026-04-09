// Copyright 2025-2026 Patrick J. Scruggs
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package client

import (
	"context"
	"fmt"
	"strings"

	tracepb "github.com/pjscruggs/slogcp-e2e-internal/services/traceproto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// DownstreamGRPCClient dials the downstream gRPC worker and exposes the
// generated TraceWorker client.
type DownstreamGRPCClient struct {
	conn   *grpc.ClientConn
	client tracepb.TraceWorkerClient
}

// NewDownstreamGRPCClient establishes a connection to the downstream gRPC
// worker. The connection uses insecure credentials by default because the test
// harness communicates with local or Cloud Run services over trusted
// networks. Additional dial options can be supplied to customize the
// connection.
func NewDownstreamGRPCClient(ctx context.Context, target string, opts ...grpc.DialOption) (*DownstreamGRPCClient, error) {
	if ctx == nil {
		return nil, fmt.Errorf("context is required")
	}
	if strings.TrimSpace(target) == "" {
		return nil, fmt.Errorf("target is required")
	}

	dialOpts := append([]grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}, opts...)

	conn, err := grpc.NewClient(target, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("dialing downstream grpc: %w", err)
	}

	return &DownstreamGRPCClient{
		conn:   conn,
		client: tracepb.NewTraceWorkerClient(conn),
	}, nil
}

// Close releases the underlying gRPC connection.
func (c *DownstreamGRPCClient) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	if err := c.conn.Close(); err != nil {
		return fmt.Errorf("closing downstream grpc connection: %w", err)
	}
	return nil
}

// Conn exposes the underlying gRPC connection for advanced use cases.
func (c *DownstreamGRPCClient) Conn() *grpc.ClientConn {
	if c == nil {
		return nil
	}
	return c.conn
}
