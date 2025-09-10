//go:build unit
// +build unit

package grpc

import (
	"bytes"
	"context"
	"io"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/pjscruggs/slogcp"
)

func TestWithTracePropagationUnaryClient(t *testing.T) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	lg, err := slogcp.New(slogcp.WithRedirectWriter(io.Discard), slogcp.WithLogTarget(slogcp.LogTargetStdout))
	if err != nil {
		t.Fatalf("slogcp.New() returned %v", err)
	}
	defer lg.Close()

	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    mustTraceID(t, "70f5c2c7b3c0d8eead4837399ac5b327"),
		SpanID:     mustSpanID(t, "5fa1c6de0d1e3e11"),
		TraceFlags: trace.FlagsSampled,
	})
	baseCtx := trace.ContextWithSpanContext(context.Background(), sc)

	tests := []struct {
		name       string
		opt        Option
		wantHeader bool
	}{
		{name: "default", opt: nil, wantHeader: true},
		{name: "disabled", opt: WithTracePropagation(false), wantHeader: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			interceptor := NewUnaryClientInterceptor(lg, tc.opt)
			invoker := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
				md, _ := metadata.FromOutgoingContext(ctx)
				tp := md.Get("traceparent")
				xc := md.Get("x-cloud-trace-context")
				if tc.wantHeader {
					if len(tp) == 0 || len(xc) == 0 {
						t.Errorf("headers missing: traceparent=%q x-cloud=%q", tp, xc)
					}
				} else {
					if len(tp) > 0 || len(xc) > 0 {
						t.Errorf("headers injected when disabled: traceparent=%q x-cloud=%q", tp, xc)
					}
				}
				return nil
			}
			if err := interceptor(baseCtx, "/svc/m", nil, nil, nil, invoker); err != nil {
				t.Fatalf("interceptor returned %v", err)
			}
		})
	}
}

func TestContextOnlyServerStream(t *testing.T) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	var buf bytes.Buffer
	lg, err := slogcp.New(slogcp.WithRedirectWriter(&buf), slogcp.WithLogTarget(slogcp.LogTargetStdout))
	if err != nil {
		t.Fatalf("slogcp.New() returned %v", err)
	}
	defer lg.Close()

	interceptor := StreamServerInterceptor(lg, WithShouldLog(func(context.Context, string) bool { return false }), WithMetadataLogging(true), WithPayloadLogging(true))

	traceHex := "70f5c2c7b3c0d8eead4837399ac5b327"
	spanHex := "5fa1c6de0d1e3e11"
	md := metadata.New(map[string]string{"traceparent": "00-" + traceHex + "-" + spanHex + "-01"})
	ctx := metadata.NewIncomingContext(context.Background(), md)
	ss := &testServerStream{ctx: ctx}

	handler := func(srv any, stream grpc.ServerStream) error {
		sc := trace.SpanContextFromContext(stream.Context())
		if sc.TraceID().String() != traceHex || sc.SpanID().String() != spanHex {
			t.Errorf("context missing trace: %v", sc)
		}
		return nil
	}
	info := &grpc.StreamServerInfo{FullMethod: "/svc/Method"}

	if err := interceptor(nil, ss, info, handler); err != nil {
		t.Fatalf("interceptor returned %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("expected no logs, got %s", buf.String())
	}
}

type testServerStream struct{ ctx context.Context }

func (tss *testServerStream) SetHeader(metadata.MD) error  { return nil }
func (tss *testServerStream) SendHeader(metadata.MD) error { return nil }
func (tss *testServerStream) SetTrailer(metadata.MD)       {}
func (tss *testServerStream) Context() context.Context     { return tss.ctx }
func (tss *testServerStream) SendMsg(interface{}) error    { return nil }
func (tss *testServerStream) RecvMsg(interface{}) error    { return nil }

func mustTraceID(t *testing.T, hex string) trace.TraceID {
	t.Helper()
	id, err := trace.TraceIDFromHex(hex)
	if err != nil {
		t.Fatalf("TraceIDFromHex(%q) returned %v", hex, err)
	}
	return id
}

func mustSpanID(t *testing.T, hex string) trace.SpanID {
	t.Helper()
	id, err := trace.SpanIDFromHex(hex)
	if err != nil {
		t.Fatalf("SpanIDFromHex(%q) returned %v", hex, err)
	}
	return id
}
