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

//go:build unit
// +build unit

package gcp

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"testing"
	"time"

	"cloud.google.com/go/logging"
)

func TestFlattenHTTPRequestToMapNil(t *testing.T) {
	if flattenHTTPRequestToMap(nil) != nil {
		t.Fatal("expected nil for nil input")
	}
}

func TestFlattenHTTPRequestToMapPopulatesFields(t *testing.T) {
	u, _ := url.Parse("https://example.com/path?q=1")
	req := &http.Request{
		Method: http.MethodGet,
		Proto:  "HTTP/2.0",
		URL:    u,
		Header: make(http.Header),
	}
	req.Header.Set("User-Agent", "agent")
	req.Header.Set("Referer", "https://ref")
	httpReq := &logging.HTTPRequest{
		Request:        req,
		RequestSize:    512,
		ResponseSize:   2048,
		Latency:        1500 * time.Millisecond,
		Status:         http.StatusAccepted,
		RemoteIP:       "203.0.113.5",
		LocalIP:        "10.1.1.2",
		CacheHit:       true,
		CacheLookup:    true,
		CacheFillBytes: 256,
	}

	payload := flattenHTTPRequestToMap(httpReq)
	if payload == nil {
		t.Fatal("payload is nil")
	}
	if payload.RequestMethod != http.MethodGet {
		t.Fatalf("method = %q, want %q", payload.RequestMethod, http.MethodGet)
	}
	if payload.Protocol != "HTTP/2.0" {
		t.Fatalf("protocol = %q, want HTTP/2.0", payload.Protocol)
	}
	if payload.RequestURL != u.String() {
		t.Fatalf("url = %q, want %q", payload.RequestURL, u.String())
	}
	if payload.Latency != "1.500000000s" {
		t.Fatalf("latency = %q, want 1.500000000s", payload.Latency)
	}
	if payload.RequestSize != "512" || payload.ResponseSize != "2048" {
		t.Fatalf("unexpected sizes: %+v", payload)
	}
	if !payload.CacheHit || !payload.CacheLookup {
		t.Fatalf("cache flags not set: %+v", payload)
	}
	if payload.CacheFillBytes != "256" {
		t.Fatalf("cacheFillBytes = %q, want 256", payload.CacheFillBytes)
	}
}

func TestResolveSlogValueHandlesSpecialCases(t *testing.T) {
	boom := errors.New("boom")
	if v := resolveSlogValue(slog.AnyValue(boom)); v != boom {
		t.Fatalf("resolveSlogValue returned %v, want original error %v", v, boom)
	}

	httpReq := &logging.HTTPRequest{}
	if v := resolveSlogValue(slog.AnyValue(httpReq)); v != nil {
		t.Fatalf("expected nil for logging.HTTPRequest, got %#v", v)
	}

	rawReq := &http.Request{}
	if v := resolveSlogValue(slog.AnyValue(rawReq)); v != nil {
		t.Fatalf("expected nil for http.Request, got %#v", v)
	}

	if v := resolveSlogValue(slog.GroupValue()); v != nil {
		t.Fatalf("empty group should return nil, got %#v", v)
	}

	groupVal := resolveSlogValue(slog.GroupValue(
		slog.String("name", "value"),
		slog.Int("number", 7),
		slog.Any("", "ignored"),
	))
	groupMap, ok := groupVal.(map[string]any)
	if !ok {
		t.Fatalf("groupVal type = %T, want map[string]any", groupVal)
	}
	if groupMap["name"] != "value" || groupMap["number"] != int64(7) {
		t.Fatalf("unexpected group contents: %#v", groupMap)
	}
	if _, exists := groupMap[""]; exists {
		t.Fatalf("empty attribute key should be omitted: %#v", groupMap)
	}
}
