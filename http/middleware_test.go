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
	"bytes"
	"context"
	"encoding/json"
	"github.com/pjscruggs/slogcp"
	"io"
	"log/slog"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

// TestMiddlewareAttachesRequestLogger verifies the middleware injects a request scope and logger.
func TestMiddlewareAttachesRequestLogger(t *testing.T) {
	var buf bytes.Buffer
	baseLogger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{AddSource: false}))

	mw := Middleware(
		WithLogger(baseLogger),
		WithProjectID("proj-123"),
		WithOTel(false),
	)

	var capturedScope *RequestScope

	handler := mw(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		scope, ok := ScopeFromContext(r.Context())
		if !ok {
			t.Fatalf("scope missing from context")
		}
		capturedScope = scope

		logger := slogcp.Logger(r.Context())
		logger.Info("processing request")
	}))

	req := httptest.NewRequest(stdhttp.MethodGet, "https://example.com/widgets?id=42", nil)
	req.RemoteAddr = "198.51.100.10:12345"
	req.Header.Set(XCloudTraceContextHeader, "105445aa7843bc8bf206b12000100000/10;o=1")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if capturedScope == nil {
		t.Fatalf("scope not captured")
	}
	if got := capturedScope.Method(); got != stdhttp.MethodGet {
		t.Fatalf("scope.Method = %q", got)
	}
	if got := capturedScope.Target(); got != "/widgets" {
		t.Fatalf("scope.Target = %q", got)
	}
	if got := capturedScope.ClientIP(); got != "198.51.100.10" {
		t.Fatalf("scope.ClientIP = %q", got)
	}
	if status := capturedScope.Status(); status != stdhttp.StatusOK {
		t.Fatalf("scope.Status = %d", status)
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 log line, got %d", len(lines))
	}

	var entry map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("unmarshal log: %v", err)
	}

	if got := entry["http.method"]; got != "GET" {
		t.Errorf("http.method = %v", got)
	}
	if got := entry["http.target"]; got != "/widgets" {
		t.Errorf("http.target = %v", got)
	}
	if _, ok := entry["http.query"]; ok {
		t.Errorf("http.query should be omitted by default")
	}
	if got := entry["network.peer.ip"]; got != "198.51.100.10" {
		t.Errorf("network.peer.ip = %v", got)
	}
	if got := entry["logging.googleapis.com/trace"]; got != "projects/proj-123/traces/105445aa7843bc8bf206b12000100000" {
		t.Errorf("trace = %v", got)
	}
	if _, ok := entry["http.latency"]; !ok {
		t.Errorf("http.latency attribute missing")
	}
}

// TestTransportInjectsTraceAndLogger ensures the transport adds trace headers and a logger.
func TestTransportInjectsTraceAndLogger(t *testing.T) {
	var buf bytes.Buffer
	baseLogger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{AddSource: false}))

	capture := &capturingRoundTripper{}
	rt := Transport(
		capture,
		WithLogger(baseLogger),
		WithProjectID("proj-123"),
		WithLegacyXCloudInjection(true),
	)

	traceID, _ := trace.TraceIDFromHex("105445aa7843bc8bf206b12000100000")
	spanID, _ := trace.SpanIDFromHex("09158d8185d3c3af")
	spanCtx := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	})

	ctx := trace.ContextWithSpanContext(context.Background(), spanCtx)

	req, err := stdhttp.NewRequestWithContext(ctx, stdhttp.MethodPost, "https://api.example.com/v1/resource", stdhttp.NoBody)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("User-Agent", "test-client/1.0")

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if resp.StatusCode != stdhttp.StatusAccepted {
		t.Fatalf("response code = %d", resp.StatusCode)
	}

	if capture.req == nil {
		t.Fatalf("captured request missing")
	}

	if got := capture.req.Header.Get("traceparent"); got == "" {
		t.Fatalf("traceparent header missing")
	}

	expectedXCTC := "105445aa7843bc8bf206b12000100000/654584908287820719;o=1"
	if got := capture.req.Header.Get(XCloudTraceContextHeader); got != expectedXCTC {
		t.Fatalf("x-cloud-trace-context = %q want %q", got, expectedXCTC)
	}

	if capture.ctx == nil {
		t.Fatalf("captured context missing")
	}

	logger := slogcp.Logger(capture.ctx)
	logger.Info("client completed")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) == 0 {
		t.Fatalf("no log output captured")
	}

	var entry map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &entry); err != nil {
		t.Fatalf("unmarshal log: %v", err)
	}

	if got := entry["http.method"]; got != "POST" {
		t.Errorf("http.method = %v", got)
	}
	if got := entry["http.host"]; got != "api.example.com" {
		t.Errorf("http.host = %v", got)
	}
	if got := entry["logging.googleapis.com/trace"]; got != "projects/proj-123/traces/105445aa7843bc8bf206b12000100000" {
		t.Errorf("trace = %v", got)
	}
}

// TestMiddlewareAttrEnricherAndTransformer ensures custom enrichers/transformers affect derived loggers.
func TestMiddlewareAttrEnricherAndTransformer(t *testing.T) {
	var buf bytes.Buffer
	baseLogger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{AddSource: false}))

	mw := Middleware(
		WithLogger(baseLogger),
		WithProjectID("proj-123"),
		WithOTel(false),
		WithAttrEnricher(func(r *stdhttp.Request, scope *RequestScope) []slog.Attr {
			return []slog.Attr{
				slog.String("tenant.id", "acme"),
				slog.String("raw.query", scope.Query()),
			}
		}),
		WithAttrTransformer(func(attrs []slog.Attr, r *stdhttp.Request, scope *RequestScope) []slog.Attr {
			filtered := make([]slog.Attr, 0, len(attrs)+1)
			for _, attr := range attrs {
				if attr.Key == "raw.query" {
					continue
				}
				filtered = append(filtered, attr)
			}
			filtered = append(filtered, slog.String("transformed", scope.Route()))
			return filtered
		}),
		WithRouteGetter(func(r *stdhttp.Request) string {
			return "/widgets/:id"
		}),
	)

	handler := mw(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		logger := slogcp.Logger(r.Context())
		logger.Info("custom attrs")
		w.WriteHeader(stdhttp.StatusNoContent)
	}))

	req := httptest.NewRequest(stdhttp.MethodGet, "https://example.com/widgets/42?color=blue", stdhttp.NoBody)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected single log line, got %d", len(lines))
	}

	var entry map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("unmarshal log: %v", err)
	}

	if got := entry["tenant.id"]; got != "acme" {
		t.Fatalf("tenant.id = %v, want acme", got)
	}
	if _, exists := entry["raw.query"]; exists {
		t.Fatalf("raw.query should have been removed by transformer")
	}
	if got := entry["transformed"]; got != "/widgets/:id" {
		t.Fatalf("transformed attr = %v, want /widgets/:id", got)
	}
}

type capturingRoundTripper struct {
	req *stdhttp.Request
	ctx context.Context
}

// RoundTrip records the request and context before returning a canned response.
func (c *capturingRoundTripper) RoundTrip(req *stdhttp.Request) (*stdhttp.Response, error) {
	c.req = req
	c.ctx = req.Context()

	body := io.NopCloser(strings.NewReader("ok"))
	return &stdhttp.Response{
		StatusCode:    stdhttp.StatusAccepted,
		Body:          body,
		ContentLength: 2,
		Request:       req,
	}, nil
}
