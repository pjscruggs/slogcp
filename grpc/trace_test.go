package grpc

import (
	"context"
	"encoding/base64"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

// TestParseGRPCTraceBin verifies grpc-trace-bin parsing succeeds and fails appropriately.
func TestParseGRPCTraceBin(t *testing.T) {
	traceID, _ := trace.TraceIDFromHex("105445aa7843bc8bf206b12000100000")
	spanID, _ := trace.SpanIDFromHex("09158d8185d3c3af")
	flags := trace.FlagsSampled

	payload := encodeTraceBin(traceID, spanID, flags)
	sc, ok := parseGRPCTraceBin(payload)
	if !ok {
		t.Fatalf("parseGRPCTraceBin() = (_, false), want true")
	}
	if sc.TraceID() != traceID || sc.SpanID() != spanID || sc.TraceFlags() != flags {
		t.Fatalf("parsed context mismatch: got %v", sc)
	}

	if _, ok := parseGRPCTraceBin("invalid-base64"); ok {
		t.Fatalf("expected invalid payload to return false")
	}
}

// TestParseXCloudTrace checks parsing of X-Cloud-Trace-Context headers.
func TestParseXCloudTrace(t *testing.T) {
	sc, ok := parseXCloudTrace("105445aa7843bc8bf206b12000100000/10;o=1")
	if !ok {
		t.Fatalf("parseXCloudTrace() = (_, false), want true")
	}
	if sc.TraceID().String() != "105445aa7843bc8bf206b12000100000" {
		t.Fatalf("traceID = %s, want 105445aa7843bc8bf206b12000100000", sc.TraceID())
	}
	if sc.SpanID().String() == "" {
		t.Fatalf("spanID missing")
	}
	if !sc.IsRemote() || !sc.IsSampled() {
		t.Fatalf("expected remote sampled span, got %#v", sc)
	}

	if _, ok := parseXCloudTrace("not-a-trace"); ok {
		t.Fatalf("expected invalid header to fail")
	}
}

// TestContextWithXCloudTrace ensures the helper augments contexts with remote spans.
func TestContextWithXCloudTrace(t *testing.T) {
	ctx := context.Background()
	newCtx, ok := contextWithXCloudTrace(ctx, "105445aa7843bc8bf206b12000100000/5;o=0")
	if !ok {
		t.Fatalf("contextWithXCloudTrace() returned false")
	}
	if !trace.SpanContextFromContext(newCtx).IsRemote() {
		t.Fatalf("expected remote span on new context")
	}

	if _, ok := contextWithXCloudTrace(ctx, ""); ok {
		t.Fatalf("empty header should not create context")
	}
}

// encodeTraceBin assembles a grpc-trace-bin payload for tests.
func encodeTraceBin(traceID trace.TraceID, spanID trace.SpanID, flags trace.TraceFlags) string {
	data := make([]byte, 0, 30)
	data = append(data, 0)
	data = append(data, 0)
	data = append(data, traceID[:]...)
	data = append(data, 1)
	data = append(data, spanID[:]...)
	data = append(data, 2)
	data = append(data, byte(flags))
	return base64.StdEncoding.EncodeToString(data)
}
