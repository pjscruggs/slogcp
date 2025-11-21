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
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestFlattenHTTPRequestToMapNormalizesEmbeddedRequest ensures flattening derives
// missing fields and formats latencies.
func TestFlattenHTTPRequestToMapNormalizesEmbeddedRequest(t *testing.T) {
	t.Parallel()

	body := strings.NewReader("payload")
	req := httptest.NewRequest(http.MethodPost, "https://example.com/upload?q=1", body)
	req.Header.Set("User-Agent", "slogcp-tests")
	req.Header.Set("Referer", "https://example.com/ref")
	req.RemoteAddr = "203.0.113.5:1234"
	req.ContentLength = 256

	httpReq := &HTTPRequest{
		Request:                        req,
		Status:                         http.StatusAccepted,
		ResponseSize:                   512,
		Latency:                        150 * time.Millisecond,
		LocalIP:                        "192.0.2.9",
		CacheHit:                       true,
		CacheLookup:                    true,
		CacheFillBytes:                 64,
		CacheValidatedWithOriginServer: true,
	}

	payload := flattenHTTPRequestToMap(httpReq)
	if payload == nil {
		t.Fatalf("flattenHTTPRequestToMap returned nil payload")
	}
	if payload.RequestMethod != http.MethodPost {
		t.Fatalf("RequestMethod = %q, want %q", payload.RequestMethod, http.MethodPost)
	}
	if payload.RequestURL != "https://example.com/upload?q=1" {
		t.Fatalf("RequestURL = %q, want full URL", payload.RequestURL)
	}
	if payload.RemoteIP != "203.0.113.5" {
		t.Fatalf("RemoteIP = %q, want stripped host", payload.RemoteIP)
	}
	if payload.RequestSize != "256" {
		t.Fatalf("RequestSize = %q, want 256", payload.RequestSize)
	}
	if payload.ResponseSize != "512" {
		t.Fatalf("ResponseSize = %q, want 512", payload.ResponseSize)
	}
	if payload.Latency != "0.150000000s" {
		t.Fatalf("Latency = %q, want formatted seconds", payload.Latency)
	}
	if payload.ServerIP != "192.0.2.9" {
		t.Fatalf("ServerIP = %q, want %q", payload.ServerIP, "192.0.2.9")
	}
	if !payload.CacheHit || !payload.CacheLookup {
		t.Fatalf("Cache flags missing: %+v", payload)
	}
}

// TestFlattenHTTPRequestToMapHandlesNil ensures nil requests short-circuit gracefully.
func TestFlattenHTTPRequestToMapHandlesNil(t *testing.T) {
	t.Parallel()

	if payload := flattenHTTPRequestToMap(nil); payload != nil {
		t.Fatalf("flattenHTTPRequestToMap(nil) = %#v, want nil", payload)
	}

	req := &HTTPRequest{
		RequestMethod: "GET",
		RequestURL:    "https://example.com",
		RequestSize:   42,
		ResponseSize:  84,
		Status:        http.StatusOK,
	}
	payload := flattenHTTPRequestToMap(req)
	if payload == nil {
		t.Fatalf("flattenHTTPRequestToMap(non-nil) returned nil")
	}
	if payload.RequestSize != "42" || payload.ResponseSize != "84" {
		t.Fatalf("size conversion failed: %#v", payload)
	}
	if payload.Latency != "" {
		t.Fatalf("Latency field should be empty when zero, got %q", payload.Latency)
	}
}

// TestResolveSlogValueCoversKinds exercises the less common slog value cases.
func TestResolveSlogValueCoversKinds(t *testing.T) {
	t.Parallel()

	group := slog.GroupValue(
		slog.String("name", "value"),
		slog.Any("skip_nil", slog.AnyValue(nil)),
	)
	groupResolved := resolveSlogValue(group)
	groupMap, ok := groupResolved.(map[string]any)
	if !ok || groupMap["name"] != "value" {
		t.Fatalf("group resolution = %#v, want map with name", groupResolved)
	}
	if _, exists := groupMap["skip_nil"]; exists {
		t.Fatalf("empty group value should be omitted: %#v", groupMap)
	}

	errVal := errors.New("explode")
	if got := resolveSlogValue(slog.AnyValue(errVal)); got != errVal.Error() {
		t.Fatalf("error resolution = %v, want %q", got, errVal.Error())
	}

	httpAttr := &HTTPRequest{RequestMethod: http.MethodGet}
	if got := resolveSlogValue(slog.AnyValue(httpAttr)); got != httpAttr {
		t.Fatalf("HTTPRequest resolution returned %v, want pointer %p", got, httpAttr)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if got := resolveSlogValue(slog.AnyValue(req)); got != nil {
		t.Fatalf("http.Request resolution = %v, want nil", got)
	}

	now := time.Unix(1700000000, 0).UTC()
	if got := resolveSlogValue(slog.TimeValue(now)); got != now.Format(time.RFC3339Nano) {
		t.Fatalf("time resolution = %v, want %q", got, now.Format(time.RFC3339Nano))
	}
	if got := resolveSlogValue(slog.BoolValue(true)); got != true {
		t.Fatalf("bool resolution = %v, want true", got)
	}
	if got := resolveSlogValue(slog.Int64Value(99)); got != int64(99) {
		t.Fatalf("int resolution = %v, want 99", got)
	}
	if got := resolveSlogValue(slog.Uint64Value(77)); got != uint64(77) {
		t.Fatalf("uint resolution = %v, want 77", got)
	}
	if got := resolveSlogValue(slog.Float64Value(3.5)); got != 3.5 {
		t.Fatalf("float resolution = %v, want 3.5", got)
	}
	if got := resolveSlogValue(slog.DurationValue(2 * time.Second)); got != (2 * time.Second).String() {
		t.Fatalf("duration resolution = %v, want %q", got, (2 * time.Second).String())
	}
	if got := resolveSlogValue(slog.StringValue("text")); got != "text" {
		t.Fatalf("string resolution = %v, want %q", got, "text")
	}

	emptyGroup := slog.GroupValue()
	if val := resolveSlogValue(emptyGroup); val != nil {
		t.Fatalf("empty group resolution = %#v, want nil", val)
	}

	groupWithBlank := slog.GroupValue(
		slog.String("", "skip"),
		slog.String("kept", "ok"),
	)
	if resolved := resolveSlogValue(groupWithBlank); resolved == nil {
		t.Fatalf("group with blank key returned nil")
	} else if group, ok := resolved.(map[string]any); !ok {
		t.Fatalf("group resolution type = %T, want map", resolved)
	} else {
		if _, exists := group[""]; exists {
			t.Fatalf("blank keys should be omitted: %#v", group)
		}
		if group["kept"] != "ok" {
			t.Fatalf("group[kept] = %v, want %q", group["kept"], "ok")
		}
	}

	groupAllNil := slog.GroupValue(
		slog.Any("only_nil", slog.AnyValue(nil)),
	)
	if val := resolveSlogValue(groupAllNil); val != nil {
		t.Fatalf("group with only nil values should resolve to nil, got %#v", val)
	}
}

// TestLabelValueToStringHandlesKinds validates conversion of different slog kinds.
func TestLabelValueToStringHandlesKinds(t *testing.T) {
	t.Parallel()

	now := time.Unix(1700000000, 0).UTC()
	duration := 42 * time.Millisecond

	tests := []struct {
		name  string
		value slog.Value
		want  string
		ok    bool
	}{
		{"string", slog.StringValue("hello"), "hello", true},
		{"int", slog.Int64Value(5), "5", true},
		{"uint", slog.Uint64Value(9), "9", true},
		{"float", slog.Float64Value(3.14), "3.14", true},
		{"bool_true", slog.BoolValue(true), "true", true},
		{"bool_false", slog.BoolValue(false), "false", true},
		{"duration", slog.DurationValue(duration), duration.String(), true},
		{"time", slog.TimeValue(now), now.Format(time.RFC3339), true},
		{"stringer", slog.AnyValue(testStringer("ok")), "ok", true},
		{"nil_any", slog.AnyValue(nil), "", false},
		{"any_default", slog.AnyValue([]int{1, 2}), "[1 2]", true},
		{"unsupported_kind", slog.GroupValue(slog.String("k", "v")), "", false},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			got, ok := labelValueToString(tt.value)
			if ok != tt.ok {
				t.Fatalf("labelValueToString(%s) ok = %v, want %v", tt.name, ok, tt.ok)
			}
			if got != tt.want {
				t.Fatalf("labelValueToString(%s) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

type testStringer string

// String returns the string value for formatting tests.
func (t testStringer) String() string { return string(t) }

// TestFormatErrorForReportingHandlesNilAndStackTracer ensures stack traces are extracted correctly.
func TestFormatErrorForReportingHandlesNilAndStackTracer(t *testing.T) {
	t.Parallel()

	fe, stack := formatErrorForReporting(nil)
	if fe.Message != "<nil error>" {
		t.Fatalf("nil error message = %q, want %q", fe.Message, "<nil error>")
	}
	if fe.Type != "" {
		t.Fatalf("nil error type = %q, want empty", fe.Type)
	}
	if stack != "" {
		t.Fatalf("nil error stack = %q, want empty", stack)
	}

	pcs := make([]uintptr, 32)
	if n := runtime.Callers(0, pcs); n > 0 {
		pcs = pcs[:n]
	}
	traceErr := &stubTracingError{pcs: pcs}

	formatted, traceStack := formatErrorForReporting(traceErr)
	if formatted.Message != traceErr.Error() {
		t.Fatalf("stack error message = %q, want %q", formatted.Message, traceErr.Error())
	}
	wantType := fmt.Sprintf("%T", traceErr)
	if formatted.Type != wantType {
		t.Fatalf("stack error type = %q, want %q", formatted.Type, wantType)
	}
	if traceStack == "" {
		t.Fatalf("expected stack trace for tracing error")
	}
	if !strings.Contains(traceStack, "TestFormatErrorForReportingHandlesNilAndStackTracer") {
		t.Fatalf("stack trace missing test frame: %q", traceStack)
	}
}

// TestCloneStringMapAndStringMapToAny ensure helper conversions copy the map content.
func TestCloneStringMapAndStringMapToAny(t *testing.T) {
	t.Parallel()

	if clone := cloneStringMap(nil); clone != nil {
		t.Fatalf("clone of nil map = %#v, want nil", clone)
	}
	if clone := cloneStringMap(map[string]string{}); clone != nil {
		t.Fatalf("clone of empty map = %#v, want nil", clone)
	}

	original := map[string]string{"env": "prod", "region": "us-central1"}
	clone := cloneStringMap(original)
	if clone == nil || clone["env"] != "prod" {
		t.Fatalf("cloneStringMap content mismatch: %#v", clone)
	}
	clone["env"] = "dev"
	if original["env"] != "prod" {
		t.Fatalf("original map mutated after clone")
	}

	anyMap := stringMapToAny(original)
	if len(anyMap) != len(original) {
		t.Fatalf("stringMapToAny size mismatch: %#v", anyMap)
	}
	if anyMap["region"] != "us-central1" {
		t.Fatalf("stringMapToAny region = %v, want %q", anyMap["region"], "us-central1")
	}
	if m := stringMapToAny(map[string]string{}); m != nil {
		t.Fatalf("stringMapToAny(empty) = %#v, want nil", m)
	}
}

// TestPrepareHTTPRequest_Nil verifies that PrepareHTTPRequest handles nil input gracefully
// and still normalizes fields when the embedded *http.Request is absent.
func TestPrepareHTTPRequest_Nil(t *testing.T) {
	t.Parallel()

	PrepareHTTPRequest(nil)

	req := &HTTPRequest{
		RequestMethod: http.MethodPatch,
		RequestURL:    "https://example.com/update",
		RequestSize:   -5,
		ResponseSize:  -10,
		RemoteIP:      "203.0.113.7:9000",
		Protocol:      "HTTP/2",
	}

	PrepareHTTPRequest(req)

	if req.Request != nil {
		t.Fatalf("Request should remain nil when not provided")
	}
	if req.RemoteIP != "203.0.113.7" {
		t.Fatalf("RemoteIP = %q, want %q", req.RemoteIP, "203.0.113.7")
	}
	if req.RequestSize != -1 || req.ResponseSize != -1 {
		t.Fatalf("size normalization = (%d, %d), want (-1, -1)", req.RequestSize, req.ResponseSize)
	}
	if req.RequestMethod != http.MethodPatch {
		t.Fatalf("RequestMethod = %q, want %q", req.RequestMethod, http.MethodPatch)
	}
	if req.RequestURL != "https://example.com/update" {
		t.Fatalf("RequestURL = %q, want %q", req.RequestURL, "https://example.com/update")
	}
	if req.Protocol != "HTTP/2" {
		t.Fatalf("Protocol = %q, want %q", req.Protocol, "HTTP/2")
	}
}

// stubTracingError implements stackTracer for exercising formatErrorForReporting.
type stubTracingError struct {
	pcs []uintptr
}

// Error returns the fixed error message.
func (e *stubTracingError) Error() string { return "trace me" }

// StackTrace exposes stored program counters to satisfy the stackTracer contract.
func (e *stubTracingError) StackTrace() []uintptr { return e.pcs }
