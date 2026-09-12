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
	"net"
	"testing"
	"time"

	"cloud.google.com/go/logging"
	logpb "cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

type loggingServer struct {
	logpb.UnimplementedLoggingServiceV2Server
	entries chan *logpb.LogEntry
}

// WriteLogEntries collects the example's application entry for assertions.
func (s *loggingServer) WriteLogEntries(_ context.Context, request *logpb.WriteLogEntriesRequest) (*logpb.WriteLogEntriesResponse, error) {
	for _, entry := range request.Entries {
		if entry.GetJsonPayload().GetFields()["message"].GetStringValue() == "service ready" {
			s.entries <- entry
		}
	}
	return &logpb.WriteLogEntriesResponse{}, nil
}

// TestEmit verifies the recipe's complete logger setup reaches a gRPC service.
func TestEmit(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	receiver := &loggingServer{entries: make(chan *logpb.LogEntry, 1)}
	logpb.RegisterLoggingServiceV2Server(server, receiver)
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	defer func() { _ = listener.Close() }()
	conn, err := grpc.NewClient("passthrough:///logging.test", grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	client, err := logging.NewClient(context.Background(), "test-project", option.WithGRPCConn(conn))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	if err := emit(context.Background(), client, "test-project"); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case entry := <-receiver.entries:
		if entry.GetJsonPayload().GetFields()["transport"].GetStringValue() != "grpc" {
			t.Fatalf("transport attribute missing %v", entry)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("example did not deliver its log")
	}
}
