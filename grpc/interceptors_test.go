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
	"errors"
	"io"
	"log/slog"
	"net"
	"reflect"
	"strings"
	"testing"

	"github.com/pjscruggs/slogcp"
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

// TestStreamServerInterceptorTracksBidi verifies streaming interceptors record metadata and sizes.
func TestStreamServerInterceptorTracksBidi(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{AddSource: false}))

	interceptor := StreamServerInterceptor(
		WithLogger(logger),
		WithProjectID("proj-123"),
	)

	md := metadata.New(map[string]string{
		XCloudTraceContextHeader: "105445aa7843bc8bf206b12000100000/5;o=1",
	})
	ctx := metadata.NewIncomingContext(context.Background(), md)
	ctx = peer.NewContext(ctx, &peer.Peer{
		Addr: &net.TCPAddr{IP: net.ParseIP("198.51.100.10"), Port: 9000},
	})

	stream := &fakeServerStream{
		ctx:       ctx,
		recvQueue: []any{&testSizedMessage{n: 128}},
	}

	var capturedInfo *RequestInfo

	handler := func(srv any, ss grpc.ServerStream) error {
		_ = slogcp.Logger(ss.Context())
		info, ok := InfoFromContext(ss.Context())
		if !ok {
			t.Fatalf("InfoFromContext missing")
		}
		capturedInfo = info

		var msg testSizedMessage
		if err := ss.RecvMsg(&msg); err != nil {
			return err
		}
		if err := ss.SendMsg(&testSizedMessage{n: 64}); err != nil {
			return err
		}
		return status.Error(codes.ResourceExhausted, "quota exceeded")
	}

	info := &grpc.StreamServerInfo{
		FullMethod:     "/example.Service/Bidi",
		IsClientStream: true,
		IsServerStream: true,
	}
	err := interceptor(nil, stream, info, handler)
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("interceptor error = %v, want ResourceExhausted", err)
	}
	if capturedInfo == nil {
		t.Fatalf("captured info missing")
	}
	if got := capturedInfo.Status(); got != codes.ResourceExhausted {
		t.Fatalf("RequestInfo.Status = %v, want ResourceExhausted", got)
	}
	if got := capturedInfo.RequestBytes(); got != 128 {
		t.Fatalf("RequestInfo.RequestBytes = %d, want 128", got)
	}
	if got := capturedInfo.ResponseBytes(); got != 64 {
		t.Fatalf("RequestInfo.ResponseBytes = %d, want 64", got)
	}

	cfg := defaultConfig()
	cfg.logger = logger
	derived := loggerWithAttrs(logger, capturedInfo.loggerAttrs(cfg, nil))
	derived.Info("after stream")
	entries := decodeStreamEntries(t, buf.String())
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	entry := entries[0]
	if got := entry["grpc.status_code"]; got != codes.ResourceExhausted.String() {
		t.Fatalf("grpc.status_code = %v, want %s (entry=%v)", got, codes.ResourceExhausted.String(), entry)
	}
	if got := entry["rpc.system"]; got != "grpc" {
		t.Fatalf("rpc.system = %v, want grpc", got)
	}
	if got := entry["rpc.request_size"]; got != float64(128) {
		t.Fatalf("rpc.request_size = %v, want 128 (entry=%v)", got, entry)
	}
	if got := entry["rpc.response_size"]; got != float64(64) {
		t.Fatalf("rpc.response_size = %v, want 64 (entry=%v)", got, entry)
	}
	if _, ok := entry["rpc.duration"]; !ok {
		t.Fatalf("rpc.duration missing")
	}
}

// TestStreamClientInterceptorTracksSizes ensures client streaming captures payload sizes and status.
func TestStreamClientInterceptorTracksSizes(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{AddSource: false}))

	interceptor := StreamClientInterceptor(
		WithLogger(logger),
		WithProjectID("proj-123"),
	)

	traceID, _ := trace.TraceIDFromHex("105445aa7843bc8bf206b12000100000")
	spanID, _ := trace.SpanIDFromHex("09158d8185d3c3af")
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	desc := &grpc.StreamDesc{StreamName: "Bidi", ClientStreams: true, ServerStreams: true}

	clientStream := &fakeClientStream{
		ctx: ctx,
		responses: []any{
			&testSizedMessage{n: 32},
		},
		recvErr: io.EOF,
	}

	var capturedInfo *RequestInfo

	streamer := func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		_ = slogcp.Logger(ctx)
		info, ok := InfoFromContext(ctx)
		if !ok {
			return nil, errors.New("missing info in context")
		}
		capturedInfo = info
		return clientStream, nil
	}

	cs, err := interceptor(ctx, desc, nil, "/example.Service/Bidi", streamer)
	if err != nil {
		t.Fatalf("StreamClientInterceptor returned %v", err)
	}

	if err := cs.SendMsg(&testSizedMessage{n: 16}); err != nil {
		t.Fatalf("SendMsg returned %v", err)
	}

	var resp testSizedMessage
	if err := cs.RecvMsg(&resp); err != nil {
		t.Fatalf("RecvMsg #1 returned %v", err)
	}

	// Second recv triggers EOF and finalization.
	if err := cs.RecvMsg(&resp); !errors.Is(err, io.EOF) {
		t.Fatalf("RecvMsg #2 = %v, want EOF", err)
	}

	if err := cs.CloseSend(); err != nil {
		t.Fatalf("CloseSend returned %v", err)
	}

	if capturedInfo.Status() != codes.OK {
		t.Fatalf("RequestInfo.Status = %v, want OK", capturedInfo.Status())
	}
	if capturedInfo.RequestBytes() != 16 {
		t.Fatalf("RequestInfo.RequestBytes = %d, want 16", capturedInfo.RequestBytes())
	}
	if capturedInfo.ResponseBytes() != 32 {
		t.Fatalf("RequestInfo.ResponseBytes = %d, want 32", capturedInfo.ResponseBytes())
	}

	cfg := defaultConfig()
	cfg.logger = logger
	derived := loggerWithAttrs(logger, capturedInfo.loggerAttrs(cfg, nil))
	derived.Info("post stream")
	entries := decodeStreamEntries(t, buf.String())
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	entry := entries[0]

	if got := entry["grpc.status_code"]; got != codes.OK.String() {
		t.Fatalf("grpc.status_code = %v, want %s (entry=%v)", got, codes.OK.String(), entry)
	}
	if got := entry["rpc.request_size"]; got != float64(16) {
		t.Fatalf("rpc.request_size = %v, want 16 (entry=%v)", got, entry)
	}
	if got := entry["rpc.response_size"]; got != float64(32) {
		t.Fatalf("rpc.response_size = %v, want 32 (entry=%v)", got, entry)
	}
}

// decodeStreamEntries converts newline-delimited JSON into a slice of maps.
func decodeStreamEntries(t *testing.T, raw string) []map[string]any {
	t.Helper()
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	lines := strings.Split(raw, "\n")
	out := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("json.Unmarshal(%q) returned %v", line, err)
		}
		out = append(out, entry)
	}
	return out
}

type testSizedMessage struct {
	n int
}

func (m *testSizedMessage) Size() int { return m.n }

type fakeServerStream struct {
	ctx       context.Context
	recvQueue []any
}

func (f *fakeServerStream) SetHeader(metadata.MD) error  { return nil }
func (f *fakeServerStream) SendHeader(metadata.MD) error { return nil }
func (f *fakeServerStream) SetTrailer(metadata.MD)       {}
func (f *fakeServerStream) Context() context.Context     { return f.ctx }
func (f *fakeServerStream) SendMsg(m any) error          { return nil }

func (f *fakeServerStream) RecvMsg(m any) error {
	if len(f.recvQueue) == 0 {
		return io.EOF
	}
	next := f.recvQueue[0]
	f.recvQueue = f.recvQueue[1:]
	return copyMessage(m, next)
}

type fakeClientStream struct {
	ctx       context.Context
	responses []any
	recvErr   error
}

func (f *fakeClientStream) Header() (metadata.MD, error) { return metadata.New(nil), nil }
func (f *fakeClientStream) Trailer() metadata.MD         { return metadata.New(nil) }
func (f *fakeClientStream) CloseSend() error             { return nil }
func (f *fakeClientStream) Context() context.Context     { return f.ctx }
func (f *fakeClientStream) SendMsg(m any) error          { return nil }

func (f *fakeClientStream) RecvMsg(m any) error {
	if len(f.responses) == 0 {
		if f.recvErr != nil {
			return f.recvErr
		}
		return io.EOF
	}
	next := f.responses[0]
	f.responses = f.responses[1:]
	return copyMessage(m, next)
}

func copyMessage(dst any, src any) error {
	switch d := dst.(type) {
	case *testSizedMessage:
		switch s := src.(type) {
		case *testSizedMessage:
			*d = *s
			return nil
		}
	}
	rdst := reflect.ValueOf(dst)
	if rdst.Kind() != reflect.Ptr {
		return errors.New("destination not pointer")
	}
	rdst.Elem().Set(reflect.ValueOf(src).Elem())
	return nil
}
