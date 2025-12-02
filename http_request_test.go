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

package slogcp

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// TestJSONHandlerCapturesHTTPRequestAttrBeforeResolution ensures buildPayload
// preserves the Cloud Logging httpRequest attribute even when attr values are
// resolved before extraction.
func TestJSONHandlerCapturesHTTPRequestAttrBeforeResolution(t *testing.T) {
	t.Parallel()

	cfg := &handlerConfig{Writer: io.Discard}
	h := newJSONHandler(cfg, slog.LevelInfo, slog.New(slog.DiscardHandler))
	state := &payloadState{}

	req := &HTTPRequest{RequestMethod: "GET", RequestURL: "https://example.com/items/1"}
	record := slog.NewRecord(time.Now(), slog.LevelInfo, "hello", 0)
	record.AddAttrs(slog.Any("httpRequest", req))

	_, captured, _, _, _, _ := h.buildPayload(record, state)
	if captured == nil {
		t.Fatalf("buildPayload did not capture httpRequest attr")
	}
	if captured.RequestMethod != "GET" || captured.RequestURL != "https://example.com/items/1" {
		t.Fatalf("captured httpRequest mismatch: %+v", captured)
	}
}

// TestHTTPRequestLogValueEnsuresCloudLoggingSchema verifies LogValue emits the expected map.
func TestHTTPRequestLogValueEnsuresCloudLoggingSchema(t *testing.T) {
	t.Parallel()

	req := &HTTPRequest{
		RequestMethod:                  http.MethodPost,
		RequestURL:                     "https://example.com/submit",
		UserAgent:                      "tester/1.0",
		Referer:                        "https://example.com/ref",
		Protocol:                       "HTTP/2",
		RequestSize:                    256,
		Status:                         http.StatusAccepted,
		ResponseSize:                   512,
		Latency:                        200 * time.Millisecond,
		RemoteIP:                       "198.51.100.1",
		LocalIP:                        "192.0.2.55",
		CacheHit:                       true,
		CacheValidatedWithOriginServer: true,
		CacheFillBytes:                 64,
		CacheLookup:                    true,
	}

	value := req.LogValue()
	if value.Kind() != slog.KindAny {
		t.Fatalf("LogValue kind = %v, want KindAny", value.Kind())
	}
	payload, ok := value.Any().(map[string]any)
	if !ok {
		t.Fatalf("LogValue payload type = %T, want map[string]any", value.Any())
	}

	checks := map[string]any{
		"requestMethod": http.MethodPost,
		"requestUrl":    "https://example.com/submit",
		"userAgent":     "tester/1.0",
		"referer":       "https://example.com/ref",
		"protocol":      "HTTP/2",
		"requestSize":   strconv.FormatInt(req.RequestSize, 10),
		"status":        http.StatusAccepted,
		"responseSize":  strconv.FormatInt(req.ResponseSize, 10),
		"remoteIp":      "198.51.100.1",
		"serverIp":      "192.0.2.55",
		"cacheHit":      true,
		"cacheLookup":   true,
	}
	for key, want := range checks {
		if got := payload[key]; got != want {
			t.Fatalf("payload[%s] = %v, want %v", key, got, want)
		}
	}
	if got := payload["cacheValidatedWithOriginServer"]; got != true {
		t.Fatalf("cacheValidatedWithOriginServer = %v, want true", got)
	}
	if got := payload["cacheFillBytes"]; got != strconv.FormatInt(req.CacheFillBytes, 10) {
		t.Fatalf("cacheFillBytes = %v, want %s", got, strconv.FormatInt(req.CacheFillBytes, 10))
	}
	if got := payload["latency"]; got != fmt.Sprintf("%.9fs", req.Latency.Seconds()) {
		t.Fatalf("latency = %v, want %q", got, fmt.Sprintf("%.9fs", req.Latency.Seconds()))
	}
}

// TestHTTPRequestPayloadValueHandlesNil ensures helper returns zero slog.Value for nil payloads.
func TestHTTPRequestPayloadValueHandlesNil(t *testing.T) {
	t.Parallel()

	value := httpRequestPayloadValue(nil)
	if value.Kind() != slog.KindAny || value.Any() != nil {
		t.Fatalf("httpRequestPayloadValue(nil) = %#v, want zero slog.Value", value)
	}
}

// TestHTTPRequestFromValueDetectsKinds exercises both acceptable slog.Value kinds.
func TestHTTPRequestFromValueDetectsKinds(t *testing.T) {
	t.Parallel()

	req := &HTTPRequest{RequestMethod: http.MethodGet}

	logValuerValue := slog.AnyValue(req)
	if logValuerValue.Kind() != slog.KindLogValuer {
		t.Fatalf("expected slog.AnyValue(req) to return KindLogValuer, got %v", logValuerValue.Kind())
	}
	if got, ok := httpRequestFromValue(logValuerValue); !ok || got != req {
		t.Fatalf("httpRequestFromValue(KindLogValuer) = (%v, %v), want (%p, true)", got, ok, req)
	}
}

// fakeLogValuer is a stub slog.LogValuer used for helper testing.
type fakeLogValuer struct{}

// LogValue implements slog.LogValuer.
func (fakeLogValuer) LogValue() slog.Value { return slog.StringValue("noop") }

// TestHTTPRequestFromLogValuerCoversHelper verifies the helper recognises HTTPRequest valuers.
func TestHTTPRequestFromLogValuerCoversHelper(t *testing.T) {
	t.Parallel()

	req := &HTTPRequest{RequestMethod: http.MethodDelete}
	if got, ok := httpRequestFromLogValuer(req); !ok || got != req {
		t.Fatalf("httpRequestFromLogValuer(*HTTPRequest) = (%v, %v), want (%p, true)", got, ok, req)
	}

	if got, ok := httpRequestFromLogValuer(fakeLogValuer{}); ok || got != nil {
		t.Fatalf("httpRequestFromLogValuer(fake) = (%v, %v), want (nil, false)", got, ok)
	}
}

// TestHTTPRequestLogValueHandlesNil ensures a nil receiver returns the zero slog.Value.
func TestHTTPRequestLogValueHandlesNil(t *testing.T) {
	t.Parallel()

	var req *HTTPRequest
	if value := req.LogValue(); value.Kind() != slog.KindAny || value.Any() != nil {
		t.Fatalf("nil receiver LogValue = %+v, want zero value", value)
	}
}
