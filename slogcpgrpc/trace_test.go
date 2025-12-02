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

package slogcpgrpc

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/metadata"

	"github.com/pjscruggs/slogcp"
)

type serverSpanContextKey struct{}

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

	validBytes, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		t.Fatalf("DecodeString(valid payload) returned %v", err)
	}
	mutate := func(fn func([]byte) []byte) string {
		b := append([]byte(nil), validBytes...)
		return base64.StdEncoding.EncodeToString(fn(b))
	}

	invalidCases := []struct {
		name string
		val  string
	}{
		{name: "empty", val: ""},
		{name: "invalid-base64", val: "not-base64"},
		{name: "short-buffer", val: base64.StdEncoding.EncodeToString(validBytes[:10])},
		{name: "bad-version", val: mutate(func(b []byte) []byte { b[0] = 1; return b })},
		{name: "bad-trace-sentinel", val: mutate(func(b []byte) []byte { b[1] = 9; return b })},
		{name: "bad-span-sentinel", val: mutate(func(b []byte) []byte { b[18] = 9; return b })},
		{name: "bad-flags-sentinel", val: mutate(func(b []byte) []byte { b[27] = 9; return b })},
		{name: "missing-flags", val: base64.StdEncoding.EncodeToString(validBytes[:len(validBytes)-1])},
	}

	for _, tt := range invalidCases {
		t.Run(tt.name, func(t *testing.T) {
			if _, ok := parseGRPCTraceBin(tt.val); ok {
				t.Fatalf("parseGRPCTraceBin(%s) unexpectedly succeeded", tt.name)
			}
		})
	}

	t.Run("invalid-trace-id", func(t *testing.T) {
		// All zeros TraceID makes the SpanContext invalid
		payload := encodeTraceBin(trace.TraceID{}, spanID, flags)
		if _, ok := parseGRPCTraceBin(payload); ok {
			t.Fatalf("parseGRPCTraceBin(zero-trace-id) unexpectedly succeeded")
		}
	})
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

// TestParseXCloudTraceFailureCases exercises error branches.
func TestParseXCloudTraceFailureCases(t *testing.T) {
	t.Parallel()

	t.Run("empty_header", func(t *testing.T) {
		if _, ok := parseXCloudTrace(""); ok {
			t.Fatalf("empty header should fail")
		}
	})

	t.Run("blank_id", func(t *testing.T) {
		if _, ok := parseXCloudTrace("   ;o=1"); ok {
			t.Fatalf("blank id should fail")
		}
	})

	t.Run("rand_error", func(t *testing.T) {
		orig := randReader
		defer func() { randReader = orig }()
		randReader = func([]byte) (int, error) {
			return 0, errors.New("entropy unavailable")
		}
		if _, ok := parseXCloudTrace("105445aa7843bc8bf206b12000100000"); ok {
			t.Fatalf("expected failure when rand.Read errors")
		}
	})

	t.Run("invalid_span_context", func(t *testing.T) {
		orig := randReader
		defer func() { randReader = orig }()
		randReader = func(b []byte) (int, error) {
			for i := range b {
				b[i] = 0
			}
			return len(b), nil
		}
		if _, ok := parseXCloudTrace("105445aa7843bc8bf206b12000100000"); ok {
			t.Fatalf("expected failure when span context remains invalid")
		}
	})
}

// TestParseXCloudTraceGeneratesSpanWhenMissing ensures missing span IDs are synthesized.
func TestParseXCloudTraceGeneratesSpanWhenMissing(t *testing.T) {
	sc, ok := parseXCloudTrace("105445aa7843bc8bf206b12000100000")
	if !ok {
		t.Fatalf("parseXCloudTrace() without span returned false")
	}
	if !sc.SpanID().IsValid() {
		t.Fatalf("expected synthesized span ID to be valid")
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

// TestMetadataCarrierAccessors exercises Get/Set/Keys.
func TestMetadataCarrierAccessors(t *testing.T) {
	md := metadata.Pairs("foo", "bar")
	carrier := metadataCarrier{MD: md}

	if got := carrier.Get("foo"); got != "bar" {
		t.Fatalf("carrier.Get(foo) = %q, want bar", got)
	}
	carrier.Set("foo", "baz")
	carrier.Set("zip", "zap")

	keys := carrier.Keys()
	want := map[string]bool{"foo": true, "zip": true}
	for _, k := range keys {
		delete(want, k)
	}
	if len(want) != 0 {
		t.Fatalf("carrier.Keys() missing keys: %v", want)
	}
	if got := md.Get("foo"); len(got) == 0 || got[0] != "baz" {
		t.Fatalf("carrier.Set() did not persist new value, got %v", got)
	}
}

// TestEnsureServerSpanContextPrefersExisting ensures an existing context short-circuits parsing.
func TestEnsureServerSpanContextPrefersExisting(t *testing.T) {
	traceID, _ := trace.TraceIDFromHex("105445aa7843bc8bf206b12000100000")
	spanID, _ := trace.SpanIDFromHex("09158d8185d3c3af")
	expected := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), expected)

	cfg := defaultConfig()
	gotCtx, sc := ensureServerSpanContext(ctx, metadata.New(nil), cfg)
	if gotCtx != ctx {
		t.Fatalf("ensureServerSpanContext returned new ctx when existing span present")
	}
	if sc.TraceID() != expected.TraceID() || sc.SpanID() != expected.SpanID() {
		t.Fatalf("span context mismatch: got %v", sc)
	}
}

// TestEnsureServerSpanContextHandlesMissingMetadata ensures the original context is preserved when nothing is extracted.
func TestEnsureServerSpanContextHandlesMissingMetadata(t *testing.T) {
	ctx := context.WithValue(context.Background(), serverSpanContextKey{}, "marker")
	tests := []struct {
		name string
		md   metadata.MD
	}{
		{name: "empty", md: metadata.New(nil)},
		{name: "nil", md: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotCtx, sc := ensureServerSpanContext(ctx, tt.md, defaultConfig())
			if gotCtx != ctx {
				t.Fatalf("expected ensureServerSpanContext to return original context when no metadata")
			}
			if sc.IsValid() {
				t.Fatalf("expected invalid span context when metadata missing, got %v", sc)
			}
		})
	}
}

// TestEnsureServerSpanContextRespectsDisabledPropagation skips extraction when disabled.
func TestEnsureServerSpanContextRespectsDisabledPropagation(t *testing.T) {
	traceID, _ := trace.TraceIDFromHex("105445aa7843bc8bf206b12000100000")
	spanID, _ := trace.SpanIDFromHex("09158d8185d3c3af")
	md := metadata.New(map[string]string{
		"traceparent": "00-" + traceID.String() + "-" + spanID.String() + "-01",
	})

	ctx := context.Background()
	cfg := &config{propagateTrace: false}

	gotCtx, sc := ensureServerSpanContext(ctx, md, cfg)
	if gotCtx != ctx {
		t.Fatalf("expected original context when propagation disabled")
	}
	if sc.IsValid() {
		t.Fatalf("expected invalid span context when propagation disabled, got %v", sc)
	}
}

// TestEnsureServerSpanContextExtractsMetadata hits each metadata fallback path.
func TestEnsureServerSpanContextExtractsMetadata(t *testing.T) {
	traceID, _ := trace.TraceIDFromHex("105445aa7843bc8bf206b12000100000")
	spanID, _ := trace.SpanIDFromHex("09158d8185d3c3af")

	origPropagator := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator())
	t.Cleanup(func() {
		otel.SetTextMapPropagator(origPropagator)
	})

	t.Run("propagator", func(t *testing.T) {
		cfg := defaultConfig()
		cfg.propagators = propagation.TraceContext{}
		md := metadata.New(nil)
		ctxWithSpan := trace.ContextWithRemoteSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
			TraceID:    traceID,
			SpanID:     spanID,
			TraceFlags: trace.FlagsSampled,
			Remote:     true,
		}))
		cfg.propagators.Inject(ctxWithSpan, metadataCarrier{md})

		_, sc := ensureServerSpanContext(context.Background(), md, cfg)
		if sc.TraceID() != traceID || sc.SpanID() != spanID {
			t.Fatalf("propagator extraction failed, got %v", sc)
		}
	})

	t.Run("grpc-trace-bin", func(t *testing.T) {
		md := metadata.New(map[string]string{
			"grpc-trace-bin": encodeTraceBin(traceID, spanID, trace.FlagsSampled),
		})
		_, sc := ensureServerSpanContext(context.Background(), md, defaultConfig())
		if sc.TraceID() != traceID || sc.SpanID() != spanID {
			t.Fatalf("grpc-trace-bin extraction failed, got %v", sc)
		}
	})

	t.Run("traceparent", func(t *testing.T) {
		md := metadata.New(map[string]string{
			"traceparent": "00-" + traceID.String() + "-" + spanID.String() + "-01",
		})
		_, sc := ensureServerSpanContext(context.Background(), md, defaultConfig())
		if sc.TraceID() != traceID || sc.SpanID() != spanID {
			t.Fatalf("traceparent extraction failed, got %v", sc)
		}
	})

	t.Run("xcloud", func(t *testing.T) {
		header := traceID.String() + "/10;o=1"
		md := metadata.Pairs(strings.ToLower(XCloudTraceContextHeader), header)
		_, sc := ensureServerSpanContext(context.Background(), md, defaultConfig())
		if sc.TraceID().String() != traceID.String() {
			t.Fatalf("xcloud extraction failed, got %v", sc)
		}
	})
}

// TestInjectClientTraceHandlesLegacyHeaders ensures injection covers both modern and legacy metadata.
func TestInjectClientTraceHandlesLegacyHeaders(t *testing.T) {
	traceID, _ := trace.TraceIDFromHex("105445aa7843bc8bf206b12000100000")
	spanID, _ := trace.SpanIDFromHex("09158d8185d3c3af")
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	}))

	md := metadata.New(nil)
	cfg := &config{
		injectLegacyXCTC: true,
		propagators:      propagation.TraceContext{},
		propagateTrace:   true,
	}

	injectClientTrace(ctx, md, cfg)
	if got := md.Get("traceparent"); len(got) == 0 {
		t.Fatalf("traceparent header missing after inject")
	}
	expected := slogcp.BuildXCloudTraceContext(traceID.String(), spanID.String(), true)
	if got := md.Get(XCloudTraceContextHeader); len(got) == 0 || got[0] != expected {
		t.Fatalf("legacy header = %v, want %s", got, expected)
	}
}

// TestInjectClientTraceSkipsWhenUnavailable ensures legacy headers are omitted without a valid span.
func TestInjectClientTraceSkipsWhenUnavailable(t *testing.T) {
	md := metadata.Pairs(strings.ToLower(XCloudTraceContextHeader), "existing")
	cfg := &config{injectLegacyXCTC: true, propagateTrace: true}

	injectClientTrace(context.Background(), md, cfg)
	if got := md.Get(XCloudTraceContextHeader); len(got) == 0 || got[0] != "existing" {
		t.Fatalf("existing legacy header should remain untouched, got %v", got)
	}

	emptyMD := metadata.New(nil)
	injectClientTrace(context.Background(), emptyMD, cfg)
	if got := emptyMD.Get(XCloudTraceContextHeader); len(got) != 0 {
		t.Fatalf("no span context should skip legacy injection, got %v", got)
	}
}

// TestInjectClientTraceRespectsExistingLegacyHeader ensures legacy headers are not overwritten when present.
func TestInjectClientTraceRespectsExistingLegacyHeader(t *testing.T) {
	traceID, _ := trace.TraceIDFromHex("105445aa7843bc8bf206b12000100000")
	spanID, _ := trace.SpanIDFromHex("09158d8185d3c3af")
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	}))

	md := metadata.Pairs(strings.ToLower(XCloudTraceContextHeader), "existing")
	cfg := &config{injectLegacyXCTC: true, propagators: propagation.TraceContext{}, propagateTrace: true}

	injectClientTrace(ctx, md, cfg)
	if got := md.Get(XCloudTraceContextHeader); len(got) == 0 || got[0] != "existing" {
		t.Fatalf("legacy header should remain untouched, got %v", got)
	}
}

// TestInjectClientTraceRespectsPropagationToggle ensures injection is skipped when disabled.
func TestInjectClientTraceRespectsPropagationToggle(t *testing.T) {
	traceID, _ := trace.TraceIDFromHex("105445aa7843bc8bf206b12000100000")
	spanID, _ := trace.SpanIDFromHex("09158d8185d3c3af")
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	}))

	md := metadata.New(nil)
	cfg := &config{
		propagateTrace:   false,
		injectLegacyXCTC: true,
		propagators:      propagation.TraceContext{},
	}

	injectClientTrace(ctx, md, cfg)
	if got := md.Len(); got != 0 {
		t.Fatalf("expected no headers injected when propagation disabled, got %d", got)
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
