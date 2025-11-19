package grpc

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/pjscruggs/slogcp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/metadata"
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
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			if _, ok := parseGRPCTraceBin(tt.val); ok {
				t.Fatalf("parseGRPCTraceBin(%s) unexpectedly succeeded", tt.name)
			}
		})
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

	cfg := &config{}
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
	ctx := context.WithValue(context.Background(), struct{}{}, "marker")
	tests := []struct {
		name string
		md   metadata.MD
	}{
		{name: "empty", md: metadata.New(nil)},
		{name: "nil", md: nil},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			gotCtx, sc := ensureServerSpanContext(ctx, tt.md, &config{})
			if gotCtx != ctx {
				t.Fatalf("expected ensureServerSpanContext to return original context when no metadata")
			}
			if sc.IsValid() {
				t.Fatalf("expected invalid span context when metadata missing, got %v", sc)
			}
		})
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
		cfg := &config{propagators: propagation.TraceContext{}}
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
		_, sc := ensureServerSpanContext(context.Background(), md, &config{})
		if sc.TraceID() != traceID || sc.SpanID() != spanID {
			t.Fatalf("grpc-trace-bin extraction failed, got %v", sc)
		}
	})

	t.Run("traceparent", func(t *testing.T) {
		md := metadata.New(map[string]string{
			"traceparent": "00-" + traceID.String() + "-" + spanID.String() + "-01",
		})
		_, sc := ensureServerSpanContext(context.Background(), md, &config{})
		if sc.TraceID() != traceID || sc.SpanID() != spanID {
			t.Fatalf("traceparent extraction failed, got %v", sc)
		}
	})

	t.Run("xcloud", func(t *testing.T) {
		header := traceID.String() + "/10;o=1"
		md := metadata.Pairs(strings.ToLower(XCloudTraceContextHeader), header)
		_, sc := ensureServerSpanContext(context.Background(), md, &config{})
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
	cfg := &config{injectLegacyXCTC: true}

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
