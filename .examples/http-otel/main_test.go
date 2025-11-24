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

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/pjscruggs/slogcp"
	"github.com/pjscruggs/slogcp/slogcphttp"
)

// TestMiddlewareUsesCustomOTelOptions ensures the example wiring exercises
// custom tracer providers, propagators, span naming, filtering, and client IP
// controls.
func TestMiddlewareUsesCustomOTelOptions(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	handler, err := slogcp.NewHandler(&buf,
		slogcp.WithSourceLocationEnabled(false),
	)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	t.Cleanup(func() {
		if cerr := handler.Close(); cerr != nil {
			t.Errorf("handler close: %v", cerr)
		}
	})

	exporter := &recordingExporter{}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exporter)),
	)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := tp.Shutdown(ctx); err != nil {
			t.Errorf("tracer shutdown: %v", err)
		}
	})

	propagator := newCountingPropagator()

	mw := slogcphttp.Middleware(
		slogcphttp.WithLogger(slog.New(handler)),
		slogcphttp.WithTracerProvider(tp),
		slogcphttp.WithPropagators(propagator),
		slogcphttp.WithPublicEndpoint(true),
		slogcphttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return r.Method + " " + r.URL.Path
		}),
		slogcphttp.WithFilter(func(r *http.Request) bool {
			return r.URL.Path != "/healthz"
		}),
		slogcphttp.WithClientIP(false),
	)

	appHandler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slogcp.Logger(r.Context()).Info("handled request")
		w.WriteHeader(http.StatusNoContent)
	}))
	healthHandler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "https://example.com/widgets/42?color=blue", nil)
	req.RemoteAddr = "203.0.113.5:4567"
	req.Header.Set("Traceparent", "00-105445aa7843bc8bf206b12000100000-09158d8185d3c3af-01")

	rec := httptest.NewRecorder()
	appHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}

	healthReq := httptest.NewRequest(http.MethodGet, "https://example.com/healthz", nil)
	recHealth := httptest.NewRecorder()
	healthHandler.ServeHTTP(recHealth, healthReq)
	if recHealth.Code != http.StatusOK {
		t.Fatalf("healthz status = %d, want %d", recHealth.Code, http.StatusOK)
	}

	if propagator.ExtractCount() < 1 {
		t.Fatalf("expected propagator to extract at least once")
	}

	spans := exporter.Spans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}

	span := spans[0]
	if got := span.Name(); got != "GET /widgets/42" {
		t.Fatalf("span.Name = %q, want %q", got, "GET /widgets/42")
	}
	if !span.SpanContext().TraceID().IsValid() {
		t.Fatalf("span trace ID is invalid")
	}

	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	if len(lines) != 1 {
		t.Fatalf("expected 1 log entry, got %d", len(lines))
	}

	var first map[string]any
	if err := json.Unmarshal(lines[0], &first); err != nil {
		t.Fatalf("unmarshal log entry: %v", err)
	}
	if _, ok := first["network.peer.ip"]; ok {
		t.Fatalf("network.peer.ip should be omitted when WithClientIP(false) is used")
	}
	if msg := first["message"]; msg != "handled request" {
		t.Fatalf("message = %v, want handled request", msg)
	}
}

type recordingExporter struct {
	mu    sync.Mutex
	spans []sdktrace.ReadOnlySpan
}

func (r *recordingExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.spans = append(r.spans, spans...)
	return nil
}

func (r *recordingExporter) Shutdown(context.Context) error { return nil }

func (r *recordingExporter) Spans() []sdktrace.ReadOnlySpan {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := make([]sdktrace.ReadOnlySpan, len(r.spans))
	copy(cp, r.spans)
	return cp
}

type countingPropagator struct {
	base    propagation.TextMapPropagator
	mu      sync.Mutex
	extract int
}

func newCountingPropagator() *countingPropagator {
	return &countingPropagator{
		base: propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{},
			propagation.Baggage{},
		),
	}
}

func (c *countingPropagator) Inject(ctx context.Context, carrier propagation.TextMapCarrier) {
	c.base.Inject(ctx, carrier)
}

func (c *countingPropagator) Extract(ctx context.Context, carrier propagation.TextMapCarrier) context.Context {
	c.mu.Lock()
	c.extract++
	c.mu.Unlock()
	return c.base.Extract(ctx, carrier)
}

func (c *countingPropagator) Fields() []string {
	return c.base.Fields()
}

func (c *countingPropagator) ExtractCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.extract
}
