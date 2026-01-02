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
	"time"

	"github.com/pjscruggs/slogcp"
)

// TestPrepareHTTPRequestNormalizesFields ensures derived values are populated and sanitized.
func TestPrepareHTTPRequestNormalizesFields(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodPost, "https://example.com/widgets/42", nil)
	req.RemoteAddr = "198.51.100.5:4321"
	req.Header.Set("User-Agent", "tester/1.0")
	req.ContentLength = 256

	payload := &slogcp.HTTPRequest{
		Request:      req,
		RequestSize:  -5,
		ResponseSize: -10,
		RemoteIP:     "198.51.100.25:8080",
	}

	slogcp.PrepareHTTPRequest(payload)

	if got := payload.RequestMethod; got != http.MethodPost {
		t.Fatalf("RequestMethod = %q, want %q", got, http.MethodPost)
	}
	if got := payload.RequestURL; got != "https://example.com/widgets/42" {
		t.Fatalf("RequestURL = %q, want https://example.com/widgets/42", got)
	}
	if got := payload.RemoteIP; got != "198.51.100.25" {
		t.Fatalf("RemoteIP = %q, want 198.51.100.25", got)
	}
	if payload.Request != nil {
		t.Fatalf("Request should be cleared after PrepareHTTPRequest")
	}
	if payload.RequestSize != -1 || payload.ResponseSize != -1 {
		t.Fatalf("normalized sizes = (%d, %d), want (-1, -1)", payload.RequestSize, payload.ResponseSize)
	}
	if got := payload.UserAgent; got != "tester/1.0" {
		t.Fatalf("UserAgent = %q, want tester/1.0", got)
	}
}

// TestHTTPRequestAttrIncorporatesScope verifies RequestScope metadata enriches the Cloud Logging payload.
func TestHTTPRequestAttrIncorporatesScope(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodPut, "https://api.example.com/things?id=99", nil)
	req.RemoteAddr = "203.0.113.17:80"

	scope := &RequestScope{
		method:      http.MethodPut,
		route:       "/things/:id",
		requestSize: 512,
		clientIP:    "203.0.113.17",
		userAgent:   "integration/1.2",
	}
	scope.status.Store(418)
	scope.latencyNS.Store(time.Second.Nanoseconds())
	scope.respBytes.Store(2048)

	attr := HTTPRequestAttr(req, scope)
	resolved := attr.Value.Resolve()
	payload, ok := resolved.Any().(map[string]any)
	if !ok {
		t.Fatalf("resolved value = %T, want map[string]any", resolved.Any())
	}

	if status, ok := payload["status"].(int); !ok || status != 418 {
		t.Fatalf("status = %#v, want 418", payload["status"])
	}
	if latency, ok := payload["latency"].(string); !ok || latency != "1.000000000s" {
		t.Fatalf("latency = %#v, want 1.000000000s", payload["latency"])
	}
	if payload["remoteIp"] != "203.0.113.17" {
		t.Fatalf("remoteIp = %#v, want 203.0.113.17", payload["remoteIp"])
	}
	if payload["requestSize"] != "512" {
		t.Fatalf("requestSize = %#v, want 512", payload["requestSize"])
	}
	if payload["responseSize"] != "2048" {
		t.Fatalf("responseSize = %#v, want 2048", payload["responseSize"])
	}
	if payload["userAgent"] != "integration/1.2" {
		t.Fatalf("userAgent = %#v, want integration/1.2", payload["userAgent"])
	}
	if payload["requestMethod"] != http.MethodPut {
		t.Fatalf("requestMethod = %#v, want PUT", payload["requestMethod"])
	}
	if payload["requestUrl"] != "https://api.example.com/things?id=99" {
		t.Fatalf("requestUrl = %#v, want original URL", payload["requestUrl"])
	}
}

// TestHTTPRequestAttrHandlesNilInputs confirms zero-value attrs when both inputs are nil.
func TestHTTPRequestAttrHandlesNilInputs(t *testing.T) {
	t.Parallel()

	attr := HTTPRequestAttr(nil, nil)
	if attr.Key != "" || attr.Value.Kind() != slog.KindAny || attr.Value.Any() != nil {
		t.Fatalf("HTTPRequestAttr(nil,nil) = %#v, want zero attr", attr)
	}
}

// TestPrepareHTTPRequestDerivesDefaults ensures missing metadata is sourced from the request.
func TestPrepareHTTPRequestDerivesDefaults(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "https://derived.example.com/parts", nil)
	req.RemoteAddr = "198.51.100.99:9000"
	req.ContentLength = 123

	payload := &slogcp.HTTPRequest{Request: req}
	slogcp.PrepareHTTPRequest(payload)

	if payload.RequestSize != req.ContentLength {
		t.Fatalf("RequestSize = %d, want %d", payload.RequestSize, req.ContentLength)
	}
	if payload.RemoteIP != "198.51.100.99" {
		t.Fatalf("RemoteIP = %q, want 198.51.100.99", payload.RemoteIP)
	}
	if payload.RequestMethod != http.MethodGet {
		t.Fatalf("RequestMethod = %q, want GET", payload.RequestMethod)
	}
	if payload.RequestURL != req.URL.String() {
		t.Fatalf("RequestURL = %q, want %q", payload.RequestURL, req.URL.String())
	}
	if payload.Request != nil {
		t.Fatalf("Request should be nil after PrepareHTTPRequest")
	}
}

// TestHTTPRequestAttrFromContext pulls the scope from context before building the attr.
func TestHTTPRequestAttrFromContext(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "https://ctx.example.com/widgets", nil)
	scope := &RequestScope{}
	ctx := context.WithValue(context.Background(), requestScopeKey{}, scope)

	attr := HTTPRequestAttrFromContext(ctx, req)
	if attr.Key != httpRequestKey {
		t.Fatalf("HTTPRequestAttrFromContext key = %q, want %q", attr.Key, httpRequestKey)
	}

	baseline := HTTPRequestAttr(req, scope)
	resolvedCtx := attr.Value.Resolve()
	resolvedBase := baseline.Value.Resolve()
	if resolvedCtx.Any() == nil || resolvedBase.Any() == nil {
		t.Fatalf("attrs should contain payloads")
	}
	if resolvedCtx.Any().(map[string]any)["requestUrl"] != resolvedBase.Any().(map[string]any)["requestUrl"] {
		t.Fatalf("RequestURL mismatch between helpers")
	}
}

// TestHTTPRequestAttrUsesRequestWhenScopeMissing ensures request data flows without scope.
func TestHTTPRequestAttrUsesRequestWhenScopeMissing(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "https://noscope.example.com/widgets?q=1", nil)
	req.RemoteAddr = "198.51.100.1:1234"

	attr := HTTPRequestAttr(req, nil)
	resolved := attr.Value.Resolve()
	payload, ok := resolved.Any().(map[string]any)
	if !ok {
		t.Fatalf("resolved payload type = %T, want map[string]any", resolved.Any())
	}
	if payload["requestMethod"] != http.MethodGet {
		t.Fatalf("requestMethod = %#v, want GET", payload["requestMethod"])
	}
	if payload["requestUrl"] != req.URL.String() {
		t.Fatalf("requestUrl = %#v, want %q", payload["requestUrl"], req.URL.String())
	}
	if payload["remoteIp"] != "198.51.100.1" {
		t.Fatalf("remoteIp = %#v, want 198.51.100.1", payload["remoteIp"])
	}
}

// TestHTTPRequestFromScopeBuildsSnapshot verifies snapshot helper emits final values.
func TestHTTPRequestFromScopeBuildsSnapshot(t *testing.T) {
	t.Parallel()

	scope := &RequestScope{
		method:      http.MethodPost,
		target:      "/items/1",
		query:       "q=ok",
		scheme:      schemeHTTPS,
		host:        "api.example.com",
		requestSize: 128,
		clientIP:    "198.51.100.5",
		userAgent:   "tester/2.0",
	}
	scope.status.Store(201)
	scope.respBytes.Store(512)
	scope.latencyNS.Store(750 * time.Millisecond.Nanoseconds())

	req := HTTPRequestFromScope(scope)
	if req == nil {
		t.Fatal("HTTPRequestFromScope returned nil")
	}
	if req.RequestURL != "https://api.example.com/items/1?q=ok" {
		t.Fatalf("RequestURL = %q, want https://api.example.com/items/1?q=ok", req.RequestURL)
	}
	if req.Status != 201 || req.ResponseSize != 512 || req.RequestSize != 128 {
		t.Fatalf("sizes/status mismatch: %+v", req)
	}
	if req.RemoteIP != "198.51.100.5" || req.UserAgent != "tester/2.0" {
		t.Fatalf("metadata mismatch: %+v", req)
	}
	if req.Latency != 750*time.Millisecond {
		t.Fatalf("Latency = %v, want 750ms", req.Latency)
	}

	// In-flight: latency should be omitted.
	scope.latencyNS.Store(unsetLatencySentinel)
	req = HTTPRequestFromScope(scope)
	if req.Latency != -1 && req.Latency >= 0 {
		t.Fatalf("Latency should be omitted when unset, got %v", req.Latency)
	}
	if req.Status != -1 {
		t.Fatalf("Status should be suppressed for in-flight requests, got %d", req.Status)
	}
	if req.ResponseSize != -1 {
		t.Fatalf("ResponseSize should be suppressed for in-flight requests, got %d", req.ResponseSize)
	}
}

// TestHTTPRequestAttrMidRequestOmitsLatency ensures mid-request helper uses sentinel latency.
func TestHTTPRequestAttrMidRequestOmitsLatency(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "https://example.com/inflight", nil)
	scope := newRequestScope(req, time.Now(), defaultConfig())
	attr := HTTPRequestAttr(nil, scope)
	payload := attr.Value.Resolve().Any().(map[string]any)
	if _, ok := payload["latency"]; ok {
		t.Fatalf("latency should be omitted for mid-request logs, got %#v", payload["latency"])
	}
	if _, ok := payload["status"]; ok {
		t.Fatalf("status should be omitted for mid-request logs, got %#v", payload["status"])
	}
	if _, ok := payload["responseSize"]; ok {
		t.Fatalf("responseSize should be omitted for mid-request logs, got %#v", payload["responseSize"])
	}
}

// TestHTTPRequestAttrRequestOnlyUsesZeroLatency ensures nil scope yields zero latency.
func TestHTTPRequestAttrRequestOnlyUsesZeroLatency(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "https://example.com/no-scope", nil)
	attr := HTTPRequestAttr(req, nil)
	payload := attr.Value.Resolve().Any().(map[string]any)
	if payload["latency"] != "0.000000000s" {
		t.Fatalf("latency = %#v, want 0.000000000s", payload["latency"])
	}
}

// TestLatencyForLoggingNilScope covers the nil scope branch.
func TestLatencyForLoggingNilScope(t *testing.T) {
	t.Parallel()

	if got := latencyForLogging(nil); got != 0 {
		t.Fatalf("latencyForLogging(nil) = %v, want 0", got)
	}
}

// TestLatencyForLoggingFinalizedScope ensures finalized latencies are returned directly.
func TestLatencyForLoggingFinalizedScope(t *testing.T) {
	t.Parallel()

	scope := &RequestScope{}
	scope.latencyNS.Store(time.Second.Nanoseconds())
	if got := latencyForLogging(scope); got != time.Second {
		t.Fatalf("latencyForLogging(finalized) = %v, want %v", got, time.Second)
	}
}

// TestHTTPRequestFromScopeHandlesNil ensures nil inputs are safe.
func TestHTTPRequestFromScopeHandlesNil(t *testing.T) {
	t.Parallel()

	if req := HTTPRequestFromScope(nil); req != nil {
		t.Fatalf("HTTPRequestFromScope(nil) = %#v, want nil", req)
	}
	if url := buildRequestURLFromScope(nil); url != "" {
		t.Fatalf("buildRequestURLFromScope(nil) = %q, want empty string", url)
	}
}

// TestHTTPRequestEnricher ensures the helper satisfies AttrEnricher.
func TestHTTPRequestEnricher(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodPost, "https://enrich.example.com/api", nil)
	scope := &RequestScope{}
	scope.status.Store(201)
	scope.latencyNS.Store(250 * time.Millisecond.Nanoseconds())

	attrs := HTTPRequestEnricher(req, scope)
	if len(attrs) != 1 {
		t.Fatalf("HTTPRequestEnricher len = %d, want 1", len(attrs))
	}
	if attrs[0].Key != httpRequestKey {
		t.Fatalf("HTTPRequestEnricher attr key = %q, want %q", attrs[0].Key, httpRequestKey)
	}

	if extra := HTTPRequestEnricher(nil, nil); len(extra) != 0 {
		t.Fatalf("HTTPRequestEnricher with nil inputs should return no attrs")
	}
}

// TestHTTPRequestFromRequest verifies the convenience constructor normalizes data.
func TestHTTPRequestFromRequest(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "https://derived.example.com/parts", nil)
	req.RemoteAddr = "198.51.100.99:9000"
	req.ContentLength = 123

	payload := slogcp.HTTPRequestFromRequest(req)
	if payload == nil {
		t.Fatalf("HTTPRequestFromRequest returned nil")
	}
	if payload.Request != nil {
		t.Fatalf("HTTPRequestFromRequest should call PrepareHTTPRequest and clear Request")
	}
	if payload.RequestURL != req.URL.String() {
		t.Fatalf("RequestURL = %q, want %q", payload.RequestURL, req.URL.String())
	}

	if payload := slogcp.HTTPRequestFromRequest(nil); payload != nil {
		t.Fatalf("HTTPRequestFromRequest(nil) should return nil, got %#v", payload)
	}
}
