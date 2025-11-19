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

package http

import (
	"context"
	"errors"
	stdhttp "net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

// TestContextWithXCloudTrace ensures a valid legacy header populates a sampled span context.
func TestContextWithXCloudTrace(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	header := "105445aa7843bc8bf206b12000100000/10;o=1"

	newCtx, ok := contextWithXCloudTrace(ctx, header)
	if !ok {
		t.Fatalf("contextWithXCloudTrace returned !ok")
	}
	if newCtx == ctx {
		t.Fatalf("expected new context when header is valid")
	}

	spanCtx := trace.SpanContextFromContext(newCtx)
	if !spanCtx.IsValid() {
		t.Fatalf("span context invalid")
	}
	if got := spanCtx.TraceID().String(); got != "105445aa7843bc8bf206b12000100000" {
		t.Fatalf("TraceID = %s", got)
	}
	if got := spanCtx.SpanID().String(); got != "000000000000000a" {
		t.Fatalf("SpanID = %s, want 10 encoded", got)
	}
	if got := spanCtx.TraceFlags(); got != trace.FlagsSampled {
		t.Fatalf("TraceFlags = %v, want sampled", got)
	}
	if !spanCtx.IsRemote() {
		t.Fatalf("expected remote span context")
	}
}

// TestContextWithXCloudTraceInvalid verifies malformed headers leave context untouched.
func TestContextWithXCloudTraceInvalid(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	const header = "not-a-valid-trace"

	newCtx, ok := contextWithXCloudTrace(ctx, header)
	if ok {
		t.Fatalf("contextWithXCloudTrace should report failure")
	}
	if newCtx != ctx {
		t.Fatalf("context should not change for invalid header")
	}
}

// TestParseXCloudTraceFailureCases covers the early return branches.
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
		orig := randRead
		defer func() { randRead = orig }()
		randRead = func([]byte) (int, error) {
			return 0, errors.New("entropy unavailable")
		}
		if _, ok := parseXCloudTrace("105445aa7843bc8bf206b12000100000"); ok {
			t.Fatalf("expected failure when randRead errors")
		}
	})

	t.Run("invalid_span_context", func(t *testing.T) {
		orig := randRead
		defer func() { randRead = orig }()
		randRead = func(b []byte) (int, error) {
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

// TestParseXCloudTraceGeneratesSpan ensures parseXCloudTrace synthesizes span IDs when omitted.
func TestParseXCloudTraceGeneratesSpan(t *testing.T) {
	t.Parallel()

	sc, ok := parseXCloudTrace("105445aa7843bc8bf206b12000100000")
	if !ok {
		t.Fatalf("parseXCloudTrace returned !ok")
	}
	if !sc.IsValid() {
		t.Fatalf("span context invalid")
	}
	if sc.SpanID() == (trace.SpanID{}) {
		t.Fatalf("span id should be synthesized when not provided")
	}
	if sc.TraceFlags() != 0 {
		t.Fatalf("TraceFlags = %v, want 0 when sampling option omitted", sc.TraceFlags())
	}
}

// TestParseXCloudTraceRejectsBadTraceID asserts invalid trace IDs lead to parse failure.
func TestParseXCloudTraceRejectsBadTraceID(t *testing.T) {
	t.Parallel()

	if sc, ok := parseXCloudTrace("zzz"); ok || sc.IsValid() {
		t.Fatalf("expected parse failure for invalid trace id")
	}
}

// TestInjectTraceContextMiddlewareExtractsHeader ensures middleware attaches spans.
func TestInjectTraceContextMiddlewareExtractsHeader(t *testing.T) {
	t.Parallel()

	var captured trace.SpanContext
	handler := InjectTraceContextMiddleware()(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		captured = trace.SpanContextFromContext(r.Context())
		w.WriteHeader(stdhttp.StatusNoContent)
	}))

	req := httptest.NewRequest(stdhttp.MethodGet, "https://example.com", nil)
	req.Header.Set(XCloudTraceContextHeader, "105445aa7843bc8bf206b12000100000/10;o=1")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if !captured.IsValid() {
		t.Fatalf("expected span context after middleware")
	}
	if captured.TraceID().String() != "105445aa7843bc8bf206b12000100000" {
		t.Fatalf("TraceID = %s", captured.TraceID())
	}
	if captured.SpanID().String() != "000000000000000a" {
		t.Fatalf("SpanID = %s, want decimal encoded 10", captured.SpanID())
	}
}

// TestInjectTraceContextMiddlewareHonorsExistingSpan ensures header is ignored when span exists.
func TestInjectTraceContextMiddlewareHonorsExistingSpan(t *testing.T) {
	t.Parallel()

	traceID, _ := trace.TraceIDFromHex("105445aa7843bc8bf206b12000100000")
	spanID, _ := trace.SpanIDFromHex("09158d8185d3c3af")
	spanCtx := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	})

	var ctxAfter trace.SpanContext
	handler := InjectTraceContextMiddleware()(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		ctxAfter = trace.SpanContextFromContext(r.Context())
		w.WriteHeader(stdhttp.StatusNoContent)
	}))

	req := httptest.NewRequest(stdhttp.MethodGet, "https://example.com", nil)
	req = req.WithContext(trace.ContextWithSpanContext(req.Context(), spanCtx))
	req.Header.Set(XCloudTraceContextHeader, "00000000000000000000000000000000/5;o=1")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if ctxAfter.TraceID() != spanCtx.TraceID() || ctxAfter.SpanID() != spanCtx.SpanID() {
		t.Fatalf("existing span context should remain, got %v", ctxAfter)
	}
}
