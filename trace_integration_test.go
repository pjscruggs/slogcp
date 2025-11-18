package slogcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/pjscruggs/slogcp"
	"go.opentelemetry.io/otel/trace"
)

// TestHandlerEmitsCloudLoggingTraceFields verifies the final JSON carries Cloud Logging keys.
func TestHandlerEmitsCloudLoggingTraceFields(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	h, err := slogcp.NewHandler(
		nil,
		slogcp.WithRedirectWriter(&buf),
		slogcp.WithTraceProjectID("proj-123"),
	)
	if err != nil {
		t.Fatalf("NewHandler() returned %v, want nil", err)
	}
	t.Cleanup(func() { _ = h.Close() })

	traceID, _ := trace.TraceIDFromHex("105445aa7843bc8bf206b12000100000")
	spanID, _ := trace.SpanIDFromHex("09158d8185d3c3af")
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	}))

	logger := slog.New(h)
	logger.InfoContext(ctx, "local span")

	entry := decodeSingleLogLine(t, buf.String())
	if got := entry["logging.googleapis.com/trace"]; got != "projects/proj-123/traces/105445aa7843bc8bf206b12000100000" {
		t.Fatalf("trace field = %v, want projects/proj-123/traces/105445aa7843bc8bf206b12000100000", got)
	}
	if got := entry["logging.googleapis.com/spanId"]; got != "09158d8185d3c3af" {
		t.Fatalf("spanId = %v, want 09158d8185d3c3af", got)
	}
	if got := entry["logging.googleapis.com/trace_sampled"]; got != true {
		t.Fatalf("trace_sampled = %v, want true", got)
	}
}

// TestHandlerOmitsSpanIDForRemoteSpans ensures remote spans suppress spanId.
func TestHandlerOmitsSpanIDForRemoteSpans(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	h, err := slogcp.NewHandler(
		nil,
		slogcp.WithRedirectWriter(&buf),
		slogcp.WithTraceProjectID("proj-123"),
	)
	if err != nil {
		t.Fatalf("NewHandler() returned %v, want nil", err)
	}
	t.Cleanup(func() { _ = h.Close() })

	traceID, _ := trace.TraceIDFromHex("105445aa7843bc8bf206b12000100000")
	spanID, _ := trace.SpanIDFromHex("09158d8185d3c3af")
	ctx := trace.ContextWithRemoteSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	}))

	logger := slog.New(h)
	logger.InfoContext(ctx, "remote span")

	entry := decodeSingleLogLine(t, buf.String())
	if got := entry["logging.googleapis.com/trace"]; got != "projects/proj-123/traces/105445aa7843bc8bf206b12000100000" {
		t.Fatalf("trace field = %v, want Cloud Logging resource", got)
	}
	if _, ok := entry["logging.googleapis.com/spanId"]; ok {
		t.Fatalf("spanId should be omitted for remote spans: %v", entry)
	}
}

// TestHandlerFallsBackToOTELTraceKeys covers logs without a known project ID.
func TestHandlerFallsBackToOTELTraceKeys(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	h, err := slogcp.NewHandler(
		nil,
		slogcp.WithRedirectWriter(&buf),
	)
	if err != nil {
		t.Fatalf("NewHandler() returned %v, want nil", err)
	}
	t.Cleanup(func() { _ = h.Close() })

	traceID, _ := trace.TraceIDFromHex("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	spanID, _ := trace.SpanIDFromHex("bbbbbbbbbbbbbbbb")
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	}))

	logger := slog.New(h)
	logger.InfoContext(ctx, "otel fallback")

	entry := decodeSingleLogLine(t, buf.String())
	if _, ok := entry["logging.googleapis.com/trace"]; ok {
		t.Fatalf("unexpected Cloud Logging trace fields present: %v", entry)
	}
	if got := entry["otel.trace_id"]; got != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("otel.trace_id = %v, want trace id", got)
	}
	if got := entry["otel.span_id"]; got != "bbbbbbbbbbbbbbbb" {
		t.Fatalf("otel.span_id = %v, want span id", got)
	}
	if got := entry["otel.trace_sampled"]; got != true {
		t.Fatalf("otel.trace_sampled = %v, want true", got)
	}
}

// decodeSingleLogLine parses the last emitted JSON record for assertions.
func decodeSingleLogLine(t *testing.T, raw string) map[string]any {
	t.Helper()
	raw = strings.TrimSpace(raw)
	if raw == "" {
		t.Fatalf("no log output")
	}
	lines := strings.Split(raw, "\n")
	var entry map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &entry); err != nil {
		t.Fatalf("json.Unmarshal() returned %v", err)
	}
	return entry
}
