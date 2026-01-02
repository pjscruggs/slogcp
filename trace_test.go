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

package slogcp

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
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

// TestBuildXCloudTraceContextUnsampled ensures the unsampled branch is exercised.
func TestBuildXCloudTraceContextUnsampled(t *testing.T) {
	t.Parallel()

	got := BuildXCloudTraceContext("105445aa7843bc8bf206b12000100000", "000000000000000a", false)
	if got != "105445aa7843bc8bf206b12000100000/10;o=0" {
		t.Fatalf("BuildXCloudTraceContext() = %q, want unsampled formatting", got)
	}
}

// TestNormalizeTraceProjectID verifies strict normalization and validation.
func TestNormalizeTraceProjectID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		input       string
		want        string
		wantChanged bool
		wantOK      bool
	}{
		{name: "empty", input: "", want: "", wantChanged: false, wantOK: false},
		{name: "valid", input: "alpha-123", want: "alpha-123", wantChanged: false, wantOK: true},
		{name: "with_prefix", input: "projects/my-project", want: "my-project", wantChanged: true, wantOK: true},
		{name: "uppercased", input: "My-Project", want: "my-project", wantChanged: true, wantOK: true},
		{name: "reject_extra_path", input: "projects/proj/extra", want: "", wantChanged: false, wantOK: false},
		{name: "reject_pattern", input: "short", want: "", wantChanged: false, wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, changed, ok := normalizeTraceProjectID(tt.input)
			if got != tt.want || changed != tt.wantChanged || ok != tt.wantOK {
				t.Fatalf("normalizeTraceProjectID(%q) = (%q, %v, %v), want (%q, %v, %v)", tt.input, got, changed, ok, tt.want, tt.wantChanged, tt.wantOK)
			}
		})
	}
}

// TestDetectTraceProjectIDFromEnvPriority ensures higher-priority variables win.
func TestDetectTraceProjectIDFromEnvPriority(t *testing.T) {
	t.Setenv("SLOGCP_PROJECT_ID", "project-id")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "gcp-project")
	t.Setenv("SLOGCP_TRACE_PROJECT_ID", "trace-id")

	if got := detectTraceProjectIDFromEnv(); got != "trace-id" {
		t.Fatalf("detectTraceProjectIDFromEnv() = %q, want %q", got, "trace-id")
	}
}

// TestDetectTraceProjectIDFromEnvSkipsInvalid ensures invalid values are ignored.
func TestDetectTraceProjectIDFromEnvSkipsInvalid(t *testing.T) {
	t.Setenv("SLOGCP_TRACE_PROJECT_ID", "projects/invalid/extra")
	t.Setenv("SLOGCP_PROJECT_ID", "valid-project")

	if got := detectTraceProjectIDFromEnv(); got != "valid-project" {
		t.Fatalf("detectTraceProjectIDFromEnv() = %q, want %q", got, "valid-project")
	}
}

// TestCachedTraceProjectIDCachesFirstValue verifies the cached helper does not reread env vars.
func TestCachedTraceProjectIDCachesFirstValue(t *testing.T) {
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

// TestTraceAttributesIgnoresInvalidProjectID ensures invalid project IDs fall back to OTel attributes.
func TestTraceAttributesIgnoresInvalidProjectID(t *testing.T) {
	resetTraceProjectEnvCache()
	t.Cleanup(resetTraceProjectEnvCache)
	t.Setenv("SLOGCP_TRACE_PROJECT_ID", "")
	t.Setenv("SLOGCP_PROJECT_ID", "")
	t.Setenv("SLOGCP_GCP_PROJECT", "")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")
	t.Setenv("GCLOUD_PROJECT", "")
	t.Setenv("GCP_PROJECT", "")

	traceID, _ := trace.TraceIDFromHex("dddddddddddddddddddddddddddddddd")
	spanID, _ := trace.SpanIDFromHex("eeeeeeeeeeeeeeee")
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	}))

	attrs, ok := TraceAttributes(ctx, "projects/invalid/extra")
	if !ok {
		t.Fatalf("TraceAttributes() ok = false, want true")
	}
	got := attrsToMap(attrs)
	if _, exists := got[TraceKey]; exists {
		t.Fatalf("TraceKey should be omitted for invalid project ID")
	}
	if got["otel.trace_id"] != "dddddddddddddddddddddddddddddddd" {
		t.Fatalf("otel.trace_id attr = %v, want raw trace id", got["otel.trace_id"])
	}
}

// TestTraceAttributesOtelFallback covers local dev scenarios with no project ID.
func TestTraceAttributesOtelFallback(t *testing.T) {
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

// TestHandlerEmitsCloudLoggingTraceFields verifies the final JSON carries Cloud Logging keys.
func TestHandlerEmitsCloudLoggingTraceFields(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	h, err := NewHandler(
		nil,
		WithRedirectWriter(&buf),
		WithTraceProjectID("proj-123"),
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
	h, err := NewHandler(
		nil,
		WithRedirectWriter(&buf),
		WithTraceProjectID("proj-123"),
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
	h, err := NewHandler(
		nil,
		WithRedirectWriter(&buf),
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

// TestTraceAttributesReturnsNilWithoutSpan ensures contexts with no spans omit fields.
func TestTraceAttributesReturnsNilWithoutSpan(t *testing.T) {
	t.Parallel()

	if attrs, ok := TraceAttributes(context.Background(), "proj-123"); ok || len(attrs) != 0 {
		t.Fatalf("TraceAttributes() without span = (%v,%v), want (nil,false)", attrs, ok)
	}
}

// TestTraceAttributesNilContext ensures nil contexts short circuit.
func TestTraceAttributesNilContext(t *testing.T) {
	var nilCtx context.Context
	if attrs, ok := TraceAttributes(nilCtx, "proj-123"); ok || len(attrs) != 0 {
		t.Fatalf("TraceAttributes(nil) = (%v,%v), want (nil,false)", attrs, ok)
	}
}

// TestTraceHelperBranches covers attribute builders and project resolution helpers.
func TestTraceHelperBranches(t *testing.T) {
	if got := resolveTraceProject("  direct "); got != "direct" {
		t.Fatalf("resolveTraceProject trimmed = %q, want %q", got, "direct")
	}

	cloudAttrs := buildTraceAttrSet("projects/p/traces/t", "raw-trace", "raw-span", true, false)
	attrMap := attrsToMap(cloudAttrs)
	if attrMap[TraceKey] != "projects/p/traces/t" {
		t.Fatalf("TraceKey = %v, want formatted trace", attrMap[TraceKey])
	}
	if _, exists := attrMap[SpanKey]; exists {
		t.Fatalf("SpanKey should be omitted when span not owned")
	}

	otelAttrs := buildTraceAttrSet("", "raw-trace", "raw-span", true, false)
	attrMap = attrsToMap(otelAttrs)
	if attrMap["otel.trace_id"] != "raw-trace" {
		t.Fatalf("otel.trace_id = %v, want raw-trace", attrMap["otel.trace_id"])
	}
	if _, exists := attrMap["otel.span_id"]; exists {
		t.Fatalf("otel.span_id should be omitted when span not owned")
	}

	resetTraceProjectEnvCache()
	t.Setenv("GCLOUD_PROJECT", "fallback-proj")
	if got := cachedTraceProjectID(); got != "fallback-proj" {
		t.Fatalf("cachedTraceProjectID fallback = %q, want fallback-proj", got)
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
