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
	"log/slog"
	stdhttp "net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pjscruggs/slogcp"
)

// TestPrepareHTTPRequestNormalizesFields ensures derived values are populated and sanitized.
func TestPrepareHTTPRequestNormalizesFields(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(stdhttp.MethodPost, "https://example.com/widgets/42", nil)
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

	if got := payload.RequestMethod; got != stdhttp.MethodPost {
		t.Fatalf("RequestMethod = %q, want %q", got, stdhttp.MethodPost)
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

	req := httptest.NewRequest(stdhttp.MethodPut, "https://api.example.com/things?id=99", nil)
	req.RemoteAddr = "203.0.113.17:80"

	scope := &RequestScope{
		method:      stdhttp.MethodPut,
		route:       "/things/:id",
		requestSize: 512,
		clientIP:    "203.0.113.17",
		userAgent:   "integration/1.2",
	}
	scope.status.Store(418)
	scope.latencyNS.Store(time.Second.Nanoseconds())
	scope.respBytes.Store(2048)

	attr := HTTPRequestAttr(req, scope)
	value, ok := attr.Value.Any().(*slogcp.HTTPRequest)
	if !ok {
		t.Fatalf("attr value = %T, want *slogcp.HTTPRequest", attr.Value.Any())
	}

	if value.Status != 418 {
		t.Fatalf("Status = %d, want 418", value.Status)
	}
	if value.Latency != time.Second {
		t.Fatalf("Latency = %v, want 1s", value.Latency)
	}
	if value.RemoteIP != "203.0.113.17" {
		t.Fatalf("RemoteIP = %s, want 203.0.113.17", value.RemoteIP)
	}
	if value.RequestSize != 512 {
		t.Fatalf("RequestSize = %d, want 512", value.RequestSize)
	}
	if value.ResponseSize != 2048 {
		t.Fatalf("ResponseSize = %d, want 2048", value.ResponseSize)
	}
	if value.UserAgent != "integration/1.2" {
		t.Fatalf("UserAgent = %q, want integration/1.2", value.UserAgent)
	}
	if value.RequestMethod != stdhttp.MethodPut {
		t.Fatalf("RequestMethod = %q, want PUT", value.RequestMethod)
	}
	if value.RequestURL != "https://api.example.com/things?id=99" {
		t.Fatalf("RequestURL = %q, want original URL", value.RequestURL)
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

	req := httptest.NewRequest(stdhttp.MethodGet, "https://derived.example.com/parts", nil)
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
	if payload.RequestMethod != stdhttp.MethodGet {
		t.Fatalf("RequestMethod = %q, want GET", payload.RequestMethod)
	}
	if payload.RequestURL != req.URL.String() {
		t.Fatalf("RequestURL = %q, want %q", payload.RequestURL, req.URL.String())
	}
	if payload.Request != nil {
		t.Fatalf("Request should be nil after PrepareHTTPRequest")
	}
}
