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

package main

import (
	"context"
	"log/slog"
	"net"
	"testing"

	slogcp "github.com/pjscruggs/slogcp"
	localtracepb "github.com/pjscruggs/slogcp-e2e-internal/services/traceproto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

const bufConnSize = 1024 * 1024

// TestParseGRPCInterceptorConfigFromEnv verifies env-driven interceptor toggles.
func TestParseGRPCInterceptorConfigFromEnv(t *testing.T) {
	t.Setenv(envLogMetadata, "false")
	t.Setenv(envLogPayload, "false")

	cfg := parseGRPCInterceptorConfig()
	if cfg.includePeer {
		t.Fatalf("expected includePeer to be false when %s=false", envLogMetadata)
	}
	if cfg.includeSizes {
		t.Fatalf("expected includeSizes to be false when %s=false", envLogPayload)
	}
}

// TestPanicWorkReturnsInternalError verifies panic requests return an error.
func TestPanicWorkReturnsInternalError(t *testing.T) {
	conn, cleanup := setupTestServer(t)
	defer cleanup()
	client := localtracepb.NewTraceWorkerClient(conn)

	_, err := client.PanicWork(context.Background(), &localtracepb.PanicWorkRequest{PanicMessage: "panic"})
	if err == nil {
		t.Fatalf("expected error from PanicWork")
	}
}

// setupTestServer starts an in-memory gRPC server and returns a connected client conn.
func setupTestServer(t *testing.T) (*grpc.ClientConn, func()) {
	t.Helper()

	handler, err := slogcp.NewHandler(nil,
		slogcp.WithLevel(slog.LevelDebug),
		slogcp.WithSourceLocationEnabled(false),
	)
	if err != nil {
		t.Fatalf("failed to create test handler: %v", err)
	}
	logger := slog.New(handler)
	cfg := parseGRPCInterceptorConfig()

	lis := bufconn.Listen(bufConnSize)
	server := newGRPCServer(logger, cfg)
	localtracepb.RegisterTraceWorkerServer(server, &traceWorkerServer{
		logger:    logger,
		projectID: "test",
	})

	go func() {
		_ = server.Serve(lis)
	}()

	dialer := func(ctx context.Context, _ string) (net.Conn, error) {
		return lis.Dial()
	}

	conn, err := grpc.NewClient(
		"bufnet",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("failed to dial bufconn: %v", err)
	}

	cleanup := func() {
		server.GracefulStop()
		_ = conn.Close()
		if err := lis.Close(); err != nil {
			t.Logf("bufconn close failed: %v", err)
		}
		_ = handler.Close()
	}

	return conn, cleanup
}
