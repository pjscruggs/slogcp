// Copyright 2025 Patrick J. Scruggs
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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/pjscruggs/slogcp"
	slogcpgrpc "github.com/pjscruggs/slogcp/slogcpgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	pb "google.golang.org/grpc/examples/helloworld/helloworld"
)

// TestGRPCExampleLogsRequest ensures the gRPC example wires interceptors that
// enrich request-scoped log output.
func TestGRPCExampleLogsRequest(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	h, err := slogcp.NewHandler(&buf)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	t.Cleanup(func() {
		if cerr := h.Close(); cerr != nil {
			t.Errorf("handler close: %v", cerr)
		}
	})

	logger := slog.New(h)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}

	server := grpc.NewServer(slogcpgrpc.ServerOptions(
		slogcpgrpc.WithLogger(logger),
	)...)
	pb.RegisterGreeterServer(server, &greeterServer{logger: logger})

	done := make(chan struct{})
	go func() {
		defer close(done)
		if serveErr := server.Serve(lis); serveErr != nil && !errors.Is(serveErr, grpc.ErrServerStopped) {
			logger.Error("grpc server error", slog.String("error", serveErr.Error()))
		}
	}()
	t.Cleanup(func() {
		server.GracefulStop()
		<-done
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dialOpts := append([]grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())},
		slogcpgrpc.DialOptions(slogcpgrpc.WithLogger(logger))...,
	)

	conn, err := grpc.DialContext(ctx, lis.Addr().String(), dialOpts...)
	if err != nil {
		t.Fatalf("DialContext: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	client := pb.NewGreeterClient(conn)
	resp, err := client.SayHello(ctx, &pb.HelloRequest{Name: "slogcp"})
	if err != nil {
		t.Fatalf("SayHello: %v", err)
	}
	if resp.Message != "Hello slogcp" {
		t.Fatalf("response message = %q, want %q", resp.Message, "Hello slogcp")
	}

	logger.Info("gRPC response", slog.String("message", resp.Message))

	entries := decodeEntries(t, &buf)

	if len(entries) < 2 {
		t.Fatalf("expected at least 2 log entries, got %d", len(entries))
	}

	first := entries[0]
	if msg := first["message"]; msg != "say hello" {
		t.Fatalf("first message = %v, want %q", msg, "say hello")
	}
	if got := first["name"]; got != "slogcp" {
		t.Fatalf("request attribute name = %v, want %q", got, "slogcp")
	}

	second := entries[len(entries)-1]
	if msg := second["message"]; msg != "gRPC response" {
		t.Fatalf("last message = %v, want %q", msg, "gRPC response")
	}
}

func decodeEntries(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()

	content := bytes.TrimSpace(buf.Bytes())
	if len(content) == 0 {
		t.Fatalf("expected log output")
	}

	lines := bytes.Split(content, []byte("\n"))
	entries := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("unmarshal log line: %v", err)
		}
		entries = append(entries, entry)
	}
	return entries
}
