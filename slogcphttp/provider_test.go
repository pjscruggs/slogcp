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

package slogcphttp

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// TestMiddlewareProviderSelection exercises real recording with pre-extracted
// remote contexts. Global OTel state requires serial execution and restoration.
func TestMiddlewareProviderSelection(t *testing.T) {
	original := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(original) })
	for _, tc := range []struct {
		name       string
		header     string
		value      string
		override   bool
		public     bool
		disabled   bool
		propagate  bool
		wantSpans  int
		wantParent bool
	}{
		{"cloud", XCloudTraceContextHeader, "105445aa7843bc8bf206b12000100000/10;o=1", false, false, false, true, 1, true},
		{"w3c", "traceparent", "00-105445aa7843bc8bf206b12000100000-000000000000000a-01", false, false, false, true, 1, true},
		{"override", XCloudTraceContextHeader, "105445aa7843bc8bf206b12000100000/10;o=1", true, false, false, true, 1, true},
		{"unsampled", XCloudTraceContextHeader, "105445aa7843bc8bf206b12000100000/10;o=0", false, false, false, true, 0, true},
		{"public", XCloudTraceContextHeader, "105445aa7843bc8bf206b12000100000/10;o=1", false, true, false, true, 1, false},
		{"disabled", XCloudTraceContextHeader, "105445aa7843bc8bf206b12000100000/10;o=1", false, false, true, true, 0, false},
		{"no_propagation", XCloudTraceContextHeader, "105445aa7843bc8bf206b12000100000/10;o=1", false, false, false, false, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			globalExporter := tracetest.NewInMemoryExporter()
			global := sdktrace.NewTracerProvider(sdktrace.WithSyncer(globalExporter), sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())))
			t.Cleanup(func() { _ = global.Shutdown(context.Background()) })
			otel.SetTracerProvider(global)
			exporter := globalExporter
			opts := []Option{WithProjectID("test-project"), WithLogger(slog.New(slog.DiscardHandler)), WithPropagators(propagation.TraceContext{}), WithPublicEndpoint(tc.public), WithOTel(!tc.disabled), WithTracePropagation(tc.propagate)}
			if tc.override {
				exporter = tracetest.NewInMemoryExporter()
				explicit := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
				t.Cleanup(func() { _ = explicit.Shutdown(context.Background()) })
				opts = append(opts, WithTracerProvider(explicit))
			}
			var active trace.SpanContext
			handler := Middleware(opts...)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				active = trace.SpanContextFromContext(r.Context())
			}))
			request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "https://example.com/", nil)
			request.Header.Set(tc.header, tc.value)
			handler.ServeHTTP(httptest.NewRecorder(), request)
			spans := exporter.GetSpans()
			if len(spans) != tc.wantSpans {
				t.Fatalf("exported %d spans, want %d", len(spans), tc.wantSpans)
			}
			if tc.override && len(globalExporter.GetSpans()) != 0 {
				t.Fatal("explicit provider leaked spans to the global provider")
			}
			if tc.disabled {
				return
			}
			if !active.IsValid() || active.IsRemote() {
				t.Fatalf("handler did not receive a local server span: %v", active)
			}
			originTrace, _ := trace.TraceIDFromHex("105445aa7843bc8bf206b12000100000")
			originSpan, _ := trace.SpanIDFromHex("000000000000000a")
			if tc.wantParent && (active.TraceID() != originTrace || active.SpanID() == originSpan) {
				t.Fatalf("server span did not continue origin with a new span ID: %v", active)
			}
			if tc.wantSpans == 0 {
				if active.IsSampled() {
					t.Fatal("unsampled parent unexpectedly produced a sampled server span")
				}
				return
			}
			span := spans[0]
			if tc.wantParent {
				if span.Parent.TraceID() != originTrace || span.Parent.SpanID() != originSpan || !span.Parent.IsRemote() {
					t.Fatalf("incorrect exported server parent: %v", span.Parent)
				}
			} else if span.Parent.IsValid() || active.TraceID() == originTrace {
				t.Fatal("root server span retained the incoming parent")
			}
			if tc.public {
				if len(span.Links) != 1 || span.Links[0].SpanContext.TraceID() != originTrace || span.Links[0].SpanContext.SpanID() != originSpan {
					t.Fatalf("incorrect public endpoint origin link: %v", span.Links)
				}
			}
		})
	}
}

// TestMiddlewareCapturesProvider checks that replacing the global SDK does not
// silently change the provider of an already composed handler.
func TestMiddlewareCapturesProvider(t *testing.T) {
	original := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(original) })
	firstExporter, secondExporter := tracetest.NewInMemoryExporter(), tracetest.NewInMemoryExporter()
	first := sdktrace.NewTracerProvider(sdktrace.WithSyncer(firstExporter))
	second := sdktrace.NewTracerProvider(sdktrace.WithSyncer(secondExporter))
	t.Cleanup(func() { _ = first.Shutdown(context.Background()); _ = second.Shutdown(context.Background()) })
	otel.SetTracerProvider(first)
	handler := Middleware(WithProjectID("test-project"))(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	otel.SetTracerProvider(second)
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil))
	if len(firstExporter.GetSpans()) != 1 || len(secondExporter.GetSpans()) != 0 {
		t.Fatal("composed handler did not retain its selected provider")
	}
}
