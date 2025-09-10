//go:build unit
// +build unit

package grpc

import (
	"strconv"
	"testing"

	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/metadata"
)

func TestParseTraceparent(t *testing.T) {
	traceHex := "70f5c2c7b3c0d8eead4837399ac5b327"
	spanHex := "5fa1c6de0d1e3e11"
	md := metadata.New(map[string]string{
		"traceparent": "00-" + traceHex + "-" + spanHex + "-01",
	})
	sc, ok := parseTraceparent(md)
	if !ok {
		t.Fatalf("parseTraceparent() returned ok=false")
	}
	if sc.TraceID().String() != traceHex || sc.SpanID().String() != spanHex || !sc.IsSampled() {
		t.Errorf("parseTraceparent() = %v", sc)
	}

	mdBad := metadata.New(map[string]string{"traceparent": "bad"})
	if _, ok := parseTraceparent(mdBad); ok {
		t.Errorf("parseTraceparent() with bad header returned ok=true")
	}
}

func TestParseXCloudTraceContext(t *testing.T) {
	traceHex := "70f5c2c7b3c0d8eead4837399ac5b327"
	spanHex := "5fa1c6de0d1e3e11"
	spanUint, err := strconv.ParseUint(spanHex, 16, 64)
	if err != nil {
		t.Fatalf("ParseUint(%q) returned %v", spanHex, err)
	}
	spanDec := strconv.FormatUint(spanUint, 10)
	md := metadata.New(map[string]string{
		"x-cloud-trace-context": traceHex + "/" + spanDec + ";o=1",
	})
	sc, ok := parseXCloudTraceContext(md)
	if !ok {
		t.Fatalf("parseXCloudTraceContext() returned ok=false")
	}
	if sc.TraceID().String() != traceHex || sc.SpanID() != mustSpanID(t, spanHex) || !sc.IsSampled() {
		t.Errorf("parseXCloudTraceContext() = %v", sc)
	}

	mdBad := metadata.New(map[string]string{"x-cloud-trace-context": "bad"})
	if _, ok := parseXCloudTraceContext(mdBad); ok {
		t.Errorf("parseXCloudTraceContext() with bad header returned ok=true")
	}
}

func TestFormatXCloudTraceContextFromSpanContext(t *testing.T) {
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    mustTraceID(t, "70f5c2c7b3c0d8eead4837399ac5b327"),
		SpanID:     mustSpanID(t, "5fa1c6de0d1e3e11"),
		TraceFlags: trace.FlagsSampled,
	})
	want := "70f5c2c7b3c0d8eead4837399ac5b327/6891007561858694673;o=1"
	if got := formatXCloudTraceContextFromSpanContext(sc); got != want {
		t.Errorf("formatXCloudTraceContextFromSpanContext() = %q, want %q", got, want)
	}
	if got := formatXCloudTraceContextFromSpanContext(trace.SpanContext{}); got != "" {
		t.Errorf("formatXCloudTraceContextFromSpanContext(invalid) = %q, want empty", got)
	}
}
