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
	"time"

	"github.com/pjscruggs/slogcp"
	"go.opentelemetry.io/otel/propagation"
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

	if gotCtx := cs.Context(); gotCtx == nil {
		t.Fatalf("client stream Context() returned nil")
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

// TestStreamKindVariants exercises the helper on all boolean combinations.
func TestStreamKindVariants(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		info *grpc.StreamServerInfo
		want string
	}{
		{"bidi", &grpc.StreamServerInfo{IsClientStream: true, IsServerStream: true}, "bidi_stream"},
		{"client_only", &grpc.StreamServerInfo{IsClientStream: true}, "client_stream"},
		{"server_only", &grpc.StreamServerInfo{IsServerStream: true}, "server_stream"},
		{"unary", &grpc.StreamServerInfo{}, "unary"},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := streamKind(tt.info); got != tt.want {
				t.Fatalf("streamKind(%s) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

// TestClientStreamKindVariants covers the client descriptor helper cases.
func TestClientStreamKindVariants(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		desc *grpc.StreamDesc
		want string
	}{
		{"bidi", &grpc.StreamDesc{ClientStreams: true, ServerStreams: true}, "bidi_stream"},
		{"client_only", &grpc.StreamDesc{ClientStreams: true}, "client_stream"},
		{"server_only", &grpc.StreamDesc{ServerStreams: true}, "server_stream"},
		{"unary", &grpc.StreamDesc{}, "unary"},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := clientStreamKind(tt.desc); got != tt.want {
				t.Fatalf("clientStreamKind(%s) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

// TestUnaryServerInterceptorAttrHooks ensures attr enrichers and transformers apply to derived loggers.
func TestUnaryServerInterceptorAttrHooks(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{AddSource: false}))

	interceptor := UnaryServerInterceptor(
		WithLogger(logger),
		WithProjectID("proj-123"),
		WithAttrEnricher(func(ctx context.Context, info *RequestInfo) []slog.Attr {
			return []slog.Attr{
				slog.String("request.full_method", info.FullMethod()),
				slog.Int64("request.count", info.RequestCount()),
			}
		}),
		WithAttrTransformer(func(ctx context.Context, attrs []slog.Attr, info *RequestInfo) []slog.Attr {
			filtered := make([]slog.Attr, 0, len(attrs)+1)
			for _, attr := range attrs {
				if attr.Key == "request.count" {
					continue
				}
				filtered = append(filtered, attr)
			}
			filtered = append(filtered, slog.String("transformed", "applied"))
			return filtered
		}),
		WithOTel(false),
	)

	handler := func(ctx context.Context, req any) (any, error) {
		slogcp.Logger(ctx).Info("attr hooks")
		return &struct{}{}, nil
	}

	_, err := interceptor(context.Background(), &struct{}{}, &grpc.UnaryServerInfo{
		FullMethod: "/example.Service/Method",
	}, handler)
	if err != nil {
		t.Fatalf("interceptor returned %v", err)
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected single log line, got %d", len(lines))
	}

	var entry map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("unmarshal log: %v", err)
	}
	if got := entry["request.full_method"]; got != "/example.Service/Method" {
		t.Fatalf("request.full_method = %v, want /example.Service/Method", got)
	}
	if _, ok := entry["request.count"]; ok {
		t.Fatalf("request.count should have been removed by transformer")
	}
	if got := entry["transformed"]; got != "applied" {
		t.Fatalf("transformed attr = %v, want applied", got)
	}
}

// TestUnaryServerInterceptorPeerControl verifies peer metadata can be disabled.
func TestUnaryServerInterceptorPeerControl(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{AddSource: false}))

	interceptor := UnaryServerInterceptor(
		WithLogger(logger),
		WithProjectID("proj-123"),
		WithPeerInfo(false),
		WithOTel(false),
	)

	handler := func(ctx context.Context, req any) (any, error) {
		slogcp.Logger(ctx).Info("no peer attr")
		return &struct{}{}, nil
	}

	ctx := peer.NewContext(context.Background(), &peer.Peer{
		Addr: &net.TCPAddr{IP: net.ParseIP("198.51.100.10"), Port: 443},
	})
	_, err := interceptor(ctx, &struct{}{}, &grpc.UnaryServerInfo{
		FullMethod: "/example.Service/Peerless",
	}, handler)
	if err != nil {
		t.Fatalf("interceptor returned %v", err)
	}

	var entry map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &entry); err != nil {
		t.Fatalf("unmarshal log: %v", err)
	}
	if _, exists := entry["net.peer.ip"]; exists {
		t.Fatalf("net.peer.ip should be omitted when WithPeerInfo(false)")
	}
}

// TestUnaryClientInterceptorPayloadSizesToggle ensures size tracking can be disabled.
func TestUnaryClientInterceptorPayloadSizesToggle(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{AddSource: false}))

	interceptor := UnaryClientInterceptor(
		WithLogger(logger),
		WithProjectID("proj-123"),
		WithPayloadSizes(false),
	)

	ctx := context.Background()
	invoker := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
		slogcp.Logger(ctx).Info("payload disabled")
		return nil
	}

	if err := interceptor(ctx, "/example.Service/NoSizes", &testSizedMessage{n: 10}, &testSizedMessage{n: 5}, nil, invoker); err != nil {
		t.Fatalf("interceptor returned %v", err)
	}

	var entry map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &entry); err != nil {
		t.Fatalf("unmarshal log: %v", err)
	}
	if _, exists := entry["rpc.request_size"]; exists {
		t.Fatalf("rpc.request_size should be omitted when payload sizes disabled")
	}
	if _, exists := entry["rpc.response_size"]; exists {
		t.Fatalf("rpc.response_size should be omitted when payload sizes disabled")
	}
}

// TestStatsHandlerOptionsHonorsConfig verifies tracer and propagator settings drive otel options.
func TestStatsHandlerOptionsHonorsConfig(t *testing.T) {
	t.Parallel()

	cfg := &config{}
	if opts := statsHandlerOptions(cfg); len(opts) != 0 {
		t.Fatalf("expected no statsHandlerOptions by default, got %d", len(opts))
	}

	cfg.tracerProvider = trace.NewNoopTracerProvider()
	cfg.propagators = propagation.TraceContext{}
	cfg.propagatorsSet = true

	if opts := statsHandlerOptions(cfg); len(opts) != 2 {
		t.Fatalf("expected tracer provider and propagator options, got %d", len(opts))
	}
}

// TestLoggerWithAttrsCoversBranches ensures nil bases and attribute copies behave as expected.
func TestLoggerWithAttrsCoversBranches(t *testing.T) {
	t.Parallel()

	base := slog.New(slog.NewJSONHandler(io.Discard, nil))
	if got := loggerWithAttrs(base, nil); got != base {
		t.Fatalf("loggerWithAttrs should return base when attrs empty")
	}

	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{AddSource: false})
	logger := slog.New(handler)
	derived := loggerWithAttrs(logger, []slog.Attr{slog.String("foo", "bar")})
	derived.Info("derived message")
	var entry map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &entry); err != nil {
		t.Fatalf("unmarshal derived log: %v", err)
	}
	if entry["foo"] != "bar" {
		t.Fatalf("derived attributes missing, got %v", entry)
	}

	if got := loggerWithAttrs(nil, []slog.Attr{slog.String("noop", "value")}); got == nil {
		t.Fatalf("loggerWithAttrs should never return nil")
	}
}

// TestClientStreamWrapperSendMsgRecordsErrors validates payload accounting and finalization on send errors.
func TestClientStreamWrapperSendMsgRecordsErrors(t *testing.T) {
	t.Parallel()

	info := newRequestInfo("/svc/ClientStream/Upload", "client_stream", true, time.Now())
	cfg := &config{includeSizes: true}
	stream := &fakeClientStream{
		ctx:     context.Background(),
		sendErr: status.Error(codes.Internal, "boom"),
	}
	wrapper := &clientStreamWrapper{
		ClientStream: stream,
		cfg:          cfg,
		info:         info,
		start:        time.Now(),
	}

	err := wrapper.SendMsg(&testSizedMessage{n: 12})
	if status.Code(err) != codes.Internal {
		t.Fatalf("SendMsg returned %v, want Internal", err)
	}
	if info.RequestBytes() != 12 || info.RequestCount() != 1 {
		t.Fatalf("request tracking incorrect: bytes=%d count=%d", info.RequestBytes(), info.RequestCount())
	}
	if info.Status() != codes.Internal {
		t.Fatalf("status = %v, want Internal", info.Status())
	}
}

// TestClientStreamWrapperRecvMsgFinalization covers EOF and error completions.
func TestClientStreamWrapperRecvMsgFinalization(t *testing.T) {
	t.Parallel()

	info := newRequestInfo("/svc/ClientStream/Recv", "client_stream", true, time.Now())
	cfg := &config{includeSizes: true}
	stream := &fakeClientStream{
		ctx:       context.Background(),
		responses: []any{&testSizedMessage{n: 5}},
	}
	wrapper := &clientStreamWrapper{
		ClientStream: stream,
		cfg:          cfg,
		info:         info,
		start:        time.Now(),
	}

	if err := wrapper.RecvMsg(&testSizedMessage{}); err != nil {
		t.Fatalf("first RecvMsg returned %v", err)
	}
	if info.ResponseCount() != 1 || info.ResponseBytes() != 5 {
		t.Fatalf("response tracking incorrect: bytes=%d count=%d", info.ResponseBytes(), info.ResponseCount())
	}

	stream.recvErr = io.EOF
	if err := wrapper.RecvMsg(&testSizedMessage{}); err != io.EOF {
		t.Fatalf("expected io.EOF, got %v", err)
	}
	if info.Status() != codes.OK {
		t.Fatalf("status after EOF = %v, want OK", info.Status())
	}

	stream.recvErr = status.Error(codes.DataLoss, "corrupt")
	wrapper = &clientStreamWrapper{
		ClientStream: stream,
		cfg:          cfg,
		info:         newRequestInfo("/svc/ClientStream/Recv", "client_stream", true, time.Now()),
		start:        time.Now(),
	}
	if err := wrapper.RecvMsg(&testSizedMessage{}); status.Code(err) != codes.DataLoss {
		t.Fatalf("expected DataLoss, got %v", err)
	}
	if wrapper.info.Status() != codes.DataLoss {
		t.Fatalf("status after error = %v, want DataLoss", wrapper.info.Status())
	}
}

// TestClientStreamWrapperCloseSendFinalizes ensures CloseSend errors finalize request info.
func TestClientStreamWrapperCloseSendFinalizes(t *testing.T) {
	t.Parallel()

	info := newRequestInfo("/svc/ClientStream/Close", "client_stream", true, time.Now())
	cfg := &config{}
	stream := &fakeClientStream{
		ctx:      context.Background(),
		closeErr: status.Error(codes.Unavailable, "closed"),
	}
	wrapper := &clientStreamWrapper{
		ClientStream: stream,
		cfg:          cfg,
		info:         info,
		start:        time.Now(),
	}

	err := wrapper.CloseSend()
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("CloseSend returned %v, want Unavailable", err)
	}
	if info.Status() != codes.Unavailable {
		t.Fatalf("status after CloseSend = %v, want Unavailable", info.Status())
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

// Size reports the encoded size used for payload accounting in tests.
func (m *testSizedMessage) Size() int { return m.n }

type fakeServerStream struct {
	ctx       context.Context
	recvQueue []any
}

// SetHeader records response headers; no-op for tests.
func (f *fakeServerStream) SetHeader(metadata.MD) error { return nil }

// SendHeader sends headers; no-op for tests.
func (f *fakeServerStream) SendHeader(metadata.MD) error { return nil }

// SetTrailer stores response trailers; no-op for tests.
func (f *fakeServerStream) SetTrailer(metadata.MD) {}

// Context returns the stream context.
func (f *fakeServerStream) Context() context.Context { return f.ctx }

// SendMsg writes outbound messages; no-op in tests.
func (f *fakeServerStream) SendMsg(m any) error { return nil }

// RecvMsg reads queued messages, mimicking client deliveries.
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
	sendErr   error
	closeErr  error
}

// Header returns captured headers for the client stream.
func (f *fakeClientStream) Header() (metadata.MD, error) { return metadata.New(nil), nil }

// Trailer returns trailers for the client stream.
func (f *fakeClientStream) Trailer() metadata.MD { return metadata.New(nil) }

// CloseSend closes the send side; no-op here.
func (f *fakeClientStream) CloseSend() error {
	if f.closeErr != nil {
		return f.closeErr
	}
	return nil
}

// Context returns the stream context.
func (f *fakeClientStream) Context() context.Context { return f.ctx }

// SendMsg queues outbound messages; configurable for tests.
func (f *fakeClientStream) SendMsg(m any) error {
	if f.sendErr != nil {
		return f.sendErr
	}
	return nil
}

// RecvMsg pops queued responses or returns the configured error.
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

// copyMessage copies test payloads into the provided destination.
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
