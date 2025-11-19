package slogcp

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

// TestSpanIDHexToDecimalCoversSuccessAndFailure ensures hex conversion succeeds and fails appropriately.
func TestSpanIDHexToDecimalCoversSuccessAndFailure(t *testing.T) {
	t.Parallel()

	if dec, ok := SpanIDHexToDecimal("000000000000000a"); !ok || dec != "10" {
		t.Fatalf("SpanIDHexToDecimal success = (%q,%v), want (\"10\",true)", dec, ok)
	}
	if _, ok := SpanIDHexToDecimal("invalid-span"); ok {
		t.Fatalf("expected invalid span ID to fail conversion")
	}
}

// TestDetectTraceProjectIDFromEnvPriority ensures higher-priority variables win.
func TestDetectTraceProjectIDFromEnvPriority(t *testing.T) {
	t.Parallel()

	t.Setenv("SLOGCP_PROJECT_ID", "project-id")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "gcp-project")
	t.Setenv("SLOGCP_TRACE_PROJECT_ID", "trace-id")

	if got := detectTraceProjectIDFromEnv(); got != "trace-id" {
		t.Fatalf("detectTraceProjectIDFromEnv() = %q, want %q", got, "trace-id")
	}
}

// TestCachedTraceProjectIDCachesFirstValue verifies the cached helper does not reread env vars.
func TestCachedTraceProjectIDCachesFirstValue(t *testing.T) {
	t.Parallel()

	resetTraceProjectEnvCache()
	t.Cleanup(resetTraceProjectEnvCache)

	t.Setenv("SLOGCP_TRACE_PROJECT_ID", "initial-project")
	if got := cachedTraceProjectID(); got != "initial-project" {
		t.Fatalf("cachedTraceProjectID() = %q, want %q", got, "initial-project")
	}

	t.Setenv("SLOGCP_TRACE_PROJECT_ID", "mutated")
	if got := cachedTraceProjectID(); got != "initial-project" {
		t.Fatalf("cachedTraceProjectID() after env change = %q, want cached %q", got, "initial-project")
	}
}

// TestTraceAttributesFormatsCloudLoggingFields ensures attributes mirror Cloud Logging requirements.
func TestTraceAttributesFormatsCloudLoggingFields(t *testing.T) {
	t.Parallel()

	traceID, _ := trace.TraceIDFromHex("105445aa7843bc8bf206b12000100000")
	spanID, _ := trace.SpanIDFromHex("09158d8185d3c3af")
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	}))

	attrs, ok := TraceAttributes(ctx, "proj-123")
	if !ok {
		t.Fatalf("TraceAttributes() ok = false, want true")
	}
	if len(attrs) == 0 {
		t.Fatalf("TraceAttributes() returned no attributes")
	}

	got := attrsToMap(attrs)
	if got[TraceKey] != "projects/proj-123/traces/105445aa7843bc8bf206b12000100000" {
		t.Fatalf("trace attr = %v", got[TraceKey])
	}
	if got[SpanKey] != "09158d8185d3c3af" {
		t.Fatalf("spanId attr = %v, want span id", got[SpanKey])
	}
	if sampled, ok := got[SampledKey].(bool); !ok || !sampled {
		t.Fatalf("trace_sampled attr = %v, want true", got[SampledKey])
	}
}

// TestTraceAttributesOmitsSpanForRemote ensures remote parents do not publish span IDs.
func TestTraceAttributesOmitsSpanForRemote(t *testing.T) {
	t.Parallel()

	traceID, _ := trace.TraceIDFromHex("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	spanID, _ := trace.SpanIDFromHex("bbbbbbbbbbbbbbbb")
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	}))

	attrs, ok := TraceAttributes(ctx, "proj-remote")
	if !ok {
		t.Fatalf("TraceAttributes() ok = false, want true")
	}
	got := attrsToMap(attrs)
	if got[TraceKey] != "projects/proj-remote/traces/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("trace attr = %v", got[TraceKey])
	}
	if _, exists := got[SpanKey]; exists {
		t.Fatalf("spanId should be omitted for remote spans: %v", got)
	}
}

// TestTraceAttributesFallsBackToEnvProjectID validates env-derived project IDs.
func TestTraceAttributesFallsBackToEnvProjectID(t *testing.T) {
	t.Parallel()

	resetTraceProjectEnvCache()
	t.Cleanup(resetTraceProjectEnvCache)

	t.Setenv("SLOGCP_TRACE_PROJECT_ID", "env-project")

	traceID, _ := trace.TraceIDFromHex("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	spanID, _ := trace.SpanIDFromHex("cccccccccccccccc")
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	}))

	attrs, ok := TraceAttributes(ctx, "")
	if !ok {
		t.Fatalf("TraceAttributes() ok = false, want true")
	}
	got := attrsToMap(attrs)
	if got[TraceKey] != "projects/env-project/traces/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Fatalf("trace attr = %v, want env-derived id", got[TraceKey])
	}
}

// TestTraceAttributesOtelFallback covers local dev scenarios with no project ID.
func TestTraceAttributesOtelFallback(t *testing.T) {
	t.Parallel()

	resetTraceProjectEnvCache()
	t.Cleanup(resetTraceProjectEnvCache)

	traceID, _ := trace.TraceIDFromHex("cccccccccccccccccccccccccccccccc")
	spanID, _ := trace.SpanIDFromHex("dddddddddddddddd")
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID,
		SpanID:  spanID,
	}))

	attrs, ok := TraceAttributes(ctx, "")
	if !ok {
		t.Fatalf("TraceAttributes() ok = false, want true")
	}
	got := attrsToMap(attrs)
	if _, exists := got[TraceKey]; exists {
		t.Fatalf("Cloud Logging trace key should be absent: %v", got)
	}
	if got["otel.trace_id"] != "cccccccccccccccccccccccccccccccc" {
		t.Fatalf("otel.trace_id = %v", got["otel.trace_id"])
	}
	if got["otel.span_id"] != "dddddddddddddddd" {
		t.Fatalf("otel.span_id = %v", got["otel.span_id"])
	}
	if sampled, ok := got["otel.trace_sampled"].(bool); ok && sampled {
		t.Fatalf("otel.trace_sampled = %v, want false", sampled)
	}
}

// TestTraceAttributesReturnsNilWithoutSpan ensures contexts with no spans omit fields.
func TestTraceAttributesReturnsNilWithoutSpan(t *testing.T) {
	t.Parallel()

	if attrs, ok := TraceAttributes(context.Background(), "proj"); ok || len(attrs) != 0 {
		t.Fatalf("TraceAttributes() without span = (%v,%v), want (nil,false)", attrs, ok)
	}
}

// resetTraceProjectEnvCache clears the cached project ID for trace tests.
func resetTraceProjectEnvCache() {
	traceProjectEnvOnce = sync.Once{}
	traceProjectEnvID = ""
}

// attrsToMap flattens attribute slices into a key/value map for assertions.
func attrsToMap(attrs []slog.Attr) map[string]any {
	m := make(map[string]any, len(attrs))
	for _, attr := range attrs {
		m[attr.Key] = attrValueAny(attr.Value)
	}
	return m
}

// attrValueAny resolves slog values into primitive Go types for comparison.
func attrValueAny(v slog.Value) any {
	rv := v.Resolve()
	switch rv.Kind() {
	case slog.KindString:
		return rv.String()
	case slog.KindBool:
		return rv.Bool()
	default:
		return rv.Any()
	}
}
