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

package grpc

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/pjscruggs/slogcp"
	"log/slog"
	"net"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// TestUnaryServerInterceptorAttachesLogger ensures the server interceptor attaches a request logger and info.
func TestUnaryServerInterceptorAttachesLogger(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{AddSource: false}))

	interceptor := UnaryServerInterceptor(
		WithLogger(logger),
		WithProjectID("proj-123"),
		WithOTel(false),
	)

	var capturedInfo *RequestInfo

	handler := func(ctx context.Context, req any) (any, error) {
		info, ok := InfoFromContext(ctx)
		if !ok {
			t.Fatalf("info missing from context")
		}
		capturedInfo = info
		slogcp.Logger(ctx).Error("server processing", slog.String("extra", "value"))
		return &struct{}{}, status.Error(codes.NotFound, "missing")
	}

	md := metadata.New(map[string]string{
		XCloudTraceContextHeader: "105445aa7843bc8bf206b12000100000/10;o=1",
	})
	ctx := metadata.NewIncomingContext(context.Background(), md)
	ctx = peer.NewContext(ctx, &peer.Peer{
		Addr: &net.TCPAddr{
			IP:   net.ParseIP("198.51.100.10"),
			Port: 443,
		},
	})

	resp, err := interceptor(ctx, &struct{}{}, &grpc.UnaryServerInfo{
		FullMethod: "/example.Service/Lookup",
	}, handler)
	if resp == nil {
		t.Fatalf("expected response placeholder")
	}
	if status.Code(err) != codes.NotFound {
		t.Fatalf("expected NotFound error, got %v", err)
	}

	if capturedInfo == nil {
		t.Fatalf("request info not captured")
	}
	if capturedInfo.Status() != codes.NotFound {
		t.Fatalf("status = %v", capturedInfo.Status())
	}
	if capturedInfo.Service() != "example.Service" {
		t.Fatalf("service = %q", capturedInfo.Service())
	}
	if capturedInfo.Method() != "Lookup" {
		t.Fatalf("method = %q", capturedInfo.Method())
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected single log line, got %d", len(lines))
	}
	var entry map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("unmarshal log: %v", err)
	}
	if got := entry["rpc.service"]; got != "example.Service" {
		t.Errorf("rpc.service = %v", got)
	}
	if got := entry["logging.googleapis.com/trace"]; got != "projects/proj-123/traces/105445aa7843bc8bf206b12000100000" {
		t.Errorf("trace = %v", got)
	}
	if got := entry["net.peer.ip"]; got != "198.51.100.10" {
		t.Errorf("net.peer.ip = %v", got)
	}
	if got := entry["extra"]; got != "value" {
		t.Errorf("extra attr missing, got %v", got)
	}
}

// TestUnaryClientInterceptorInjectsTrace verifies the client interceptor injects trace metadata and logs attributes.
func TestUnaryClientInterceptorInjectsTrace(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{AddSource: false}))

	interceptor := UnaryClientInterceptor(
		WithLogger(logger),
		WithProjectID("proj-123"),
		WithLegacyXCloudInjection(true),
	)

	traceID, _ := trace.TraceIDFromHex("105445aa7843bc8bf206b12000100000")
	spanID, _ := trace.SpanIDFromHex("09158d8185d3c3af")
	spanCtx := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), spanCtx)

	var capturedMD metadata.MD
	invoker := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
		md, ok := metadata.FromOutgoingContext(ctx)
		if !ok {
			t.Fatalf("metadata missing in outgoing context")
		}
		capturedMD = md.Copy()
		slogcp.Logger(ctx).Info("client call")
		return nil
	}

	err := interceptor(ctx, "/example.Service/Lookup", &struct{}{}, &struct{}{}, nil, invoker)
	if err != nil {
		t.Fatalf("interceptor returned error: %v", err)
	}

	if capturedMD == nil {
		t.Fatalf("expected metadata capture")
	}
	if got := capturedMD.Get("traceparent"); len(got) == 0 {
		t.Fatalf("traceparent header missing")
	}
	if got := capturedMD.Get(XCloudTraceContextHeader); len(got) == 0 {
		t.Fatalf("x-cloud-trace-context header missing")
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) == 0 {
		t.Fatalf("expected log output")
	}
	var entry map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &entry); err != nil {
		t.Fatalf("unmarshal log: %v", err)
	}
	if got := entry["rpc.system"]; got != "grpc" {
		t.Errorf("rpc.system = %v", got)
	}
	if got := entry["rpc.method"]; got != "Lookup" {
		t.Errorf("rpc.method = %v", got)
	}
	if got := entry["logging.googleapis.com/trace"]; got != "projects/proj-123/traces/105445aa7843bc8bf206b12000100000" {
		t.Errorf("trace = %v", got)
	}
}
