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
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/pjscruggs/slogcp/healthcheck"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

func TestMiddlewareSuppressUnsampledBelowRespectsServerErrors(t *testing.T) {
	previous := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTextMapPropagator(previous)
	})

	logger, records := newRecordingLogger()
	mw := Middleware(logger, WithSuppressUnsampledBelow(slog.Level(30)))

	req := httptest.NewRequest(http.MethodGet, "http://example.com/ok", nil)
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00")

	rr := httptest.NewRecorder()
	mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rr, req)

	if count := records.Count(); count != 0 {
		t.Fatalf("unsampled 200 request logged %d records, want 0", count)
	}

	req = httptest.NewRequest(http.MethodGet, "http://example.com/error", nil)
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00")
	rr = httptest.NewRecorder()

	mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})).ServeHTTP(rr, req)

	if count := records.Count(); count != 1 {
		t.Fatalf("unsampled 500 request logged %d records, want 1", count)
	}
}

func TestMiddlewareChatterDrop(t *testing.T) {
	logger, records := newRecordingLogger()
	cfg := healthcheck.DefaultConfig()
	cfg.Mode = healthcheck.ModeOn
	cfg.HTTP.Paths = []string{"/healthz"}
	cfg.Action = healthcheck.ActionDrop

	mw := Middleware(logger, WithChatterConfig(cfg))

	req := httptest.NewRequest(http.MethodGet, "http://example.com/healthz", nil)
	rr := httptest.NewRecorder()

	mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rr, req)

	if rr.Result().StatusCode != http.StatusOK {
		t.Fatalf("response code = %d, want %d", rr.Result().StatusCode, http.StatusOK)
	}
	if count := records.Count(); count != 0 {
		t.Fatalf("expected no logs for dropped health check, got %d", count)
	}
}

func TestMiddlewareChatterSafetyRailOnError(t *testing.T) {
	logger, records := newRecordingLogger()
	cfg := healthcheck.DefaultConfig()
	cfg.Mode = healthcheck.ModeOn
	cfg.HTTP.Paths = []string{"/healthz"}

	mw := Middleware(logger, WithChatterConfig(cfg))

	req := httptest.NewRequest(http.MethodGet, "http://example.com/healthz", nil)
	rr := httptest.NewRecorder()

	mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})).ServeHTTP(rr, req)

	snap := records.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected safety rail to force log, got %d entries", len(snap))
	}
	rec := snap[0]
	attrMap := recordAttrsToMap(rec)
	if v, ok := attrMap["observability.chatter.safety_rail"]; !ok || v.String() != "error" {
		t.Fatalf("safety rail annotation missing or wrong: %#v", attrMap)
	}
}

func TestMiddlewareChatterCronMarkAnnotations(t *testing.T) {
	logger, records := newRecordingLogger()
	cfg := healthcheck.DefaultConfig()
	cfg.Mode = healthcheck.ModeOn

	mw := Middleware(logger, WithChatterConfig(cfg))

	req := httptest.NewRequest(http.MethodGet, "http://example.com/task", nil)
	req.Header.Set("X-Appengine-Cron", "true")
	rr := httptest.NewRecorder()

	mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rr, req)

	snap := records.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected exactly one log entry, got %d", len(snap))
	}
	rec := snap[0]
	attrMap := recordAttrsToMap(rec)
	if v := attrMap["observability.chatter.decision"].String(); v != "would_suppress" {
		t.Fatalf("decision annotation = %q, want would_suppress", v)
	}
	if v := attrMap["observability.chatter.reason"].String(); v != "appengine_cron" {
		t.Fatalf("reason annotation = %q, want appengine_cron", v)
	}
	if v := attrMap["observability.chatter.rule"].String(); v != "X-Appengine-Cron" {
		t.Fatalf("rule annotation = %q, want X-Appengine-Cron", v)
	}
}

type recordingState struct {
	mu      sync.Mutex
	records []slog.Record
}

func newRecordingLogger() (*slog.Logger, *recordingState) {
	state := &recordingState{}
	logger := slog.New(&recordingHandler{state: state})
	return logger, state
}

type recordingHandler struct {
	state *recordingState
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool {
	return true
}

func (h *recordingHandler) Handle(_ context.Context, record slog.Record) error {
	clone := slog.NewRecord(record.Time, record.Level, record.Message, record.PC)
	record.Attrs(func(attr slog.Attr) bool {
		clone.AddAttrs(attr)
		return true
	})
	h.state.mu.Lock()
	h.state.records = append(h.state.records, clone)
	h.state.mu.Unlock()
	return nil
}

func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler {
	return &recordingHandler{state: h.state}
}

func (h *recordingHandler) WithGroup(string) slog.Handler {
	return &recordingHandler{state: h.state}
}

func (s *recordingState) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.records)
}

func (s *recordingState) Snapshot() []slog.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	copied := make([]slog.Record, len(s.records))
	copy(copied, s.records)
	return copied
}

func TestResolveRemoteIP(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	req.RemoteAddr = "192.0.2.1:443"
	if got := resolveRemoteIP(req, false, nil); got != "192.0.2.1" {
		t.Fatalf("resolveRemoteIP without proxy headers = %q, want 192.0.2.1", got)
	}

	req = httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	req.RemoteAddr = "198.51.100.10:1234"
	req.Header.Add("X-Forwarded-For", " , , 203.0.113.5 , 10.0.0.1")
	req.Header.Add("X-Forwarded-For", " 198.51.100.9")
	if got := resolveRemoteIP(req, true, nil); got != "203.0.113.5" {
		t.Fatalf("resolveRemoteIP with X-Forwarded-For = %q, want 203.0.113.5", got)
	}

	req = httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	req.RemoteAddr = "198.51.100.10:1234"
	req.Header.Set("X-Real-IP", "2001:db8::1")
	if got := resolveRemoteIP(req, true, nil); got != "2001:db8::1" {
		t.Fatalf("resolveRemoteIP with X-Real-IP = %q, want 2001:db8::1", got)
	}
}

func TestHeadersGroupAttr(t *testing.T) {
	header := http.Header{}
	header.Add("X-Test", "one")
	header.Add("X-Test", "two")
	header.Set("X-Other", "value")

	attr, ok := headersGroupAttr("requestHeaders", header, []string{"X-Test", "Missing", "", "X-Other"})
	if !ok {
		t.Fatal("expected headersGroupAttr to return attr")
	}
	if attr.Key != "requestHeaders" {
		t.Fatalf("attr key = %q, want requestHeaders", attr.Key)
	}
	group := attr.Value.Group()
	if len(group) != 2 {
		t.Fatalf("group len = %d, want 2", len(group))
	}
	for _, child := range group {
		values, ok := child.Value.Any().([]string)
		if !ok {
			t.Fatalf("child %q value type = %T, want []string", child.Key, child.Value.Any())
		}
		original := append([]string(nil), values...)
		if strings.EqualFold(child.Key, "X-Test") {
			if len(values) != 2 || values[0] != "one" || values[1] != "two" {
				t.Fatalf("X-Test values = %v, want [one two]", values)
			}
			header["X-Test"][0] = "mutated"
			if values[0] != original[0] {
				t.Fatal("values slice should be copied; mutation propagated")
			}
		}
		if strings.EqualFold(child.Key, "X-Other") && (len(values) != 1 || values[0] != "value") {
			t.Fatalf("X-Other values = %v, want [value]", values)
		}
	}

	if _, ok := headersGroupAttr("empty", header, nil); ok {
		t.Fatal("expected ok=false when keys slice empty")
	}
	if _, ok := headersGroupAttr("missing", header, []string{"Not-Present"}); ok {
		t.Fatal("expected ok=false when no headers match")
	}
}

func TestMiddlewareBodyPreviewClamp(t *testing.T) {
	logger, records := newRecordingLogger()
	body := strings.Repeat("x", bodyPreviewInlineLimit+64)

	mw := Middleware(
		logger,
		WithRequestBodyLimit(int64(len(body))),
		WithResponseBodyLimit(int64(len(body))),
	)

	req := httptest.NewRequest(http.MethodPost, "http://example.com/", strings.NewReader(body))
	rr := httptest.NewRecorder()

	mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Fatalf("failed to drain request: %v", err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatalf("failed to write response: %v", err)
		}
	})).ServeHTTP(rr, req)

	snapshot := records.Snapshot()
	if len(snapshot) != 1 {
		t.Fatalf("expected exactly one log record, got %d", len(snapshot))
	}
	attrMap := recordAttrsToMap(snapshot[0])

	requestPreview, ok := attrMap["requestBody"]
	if !ok {
		t.Fatal("requestBody attribute missing")
	}
	if requestPreview.Kind() != slog.KindString {
		t.Fatalf("requestBody kind = %v, want KindString", requestPreview.Kind())
	}
	if gotLen := len(requestPreview.String()); gotLen != bodyPreviewInlineLimit {
		t.Fatalf("requestBody preview length = %d, want %d", gotLen, bodyPreviewInlineLimit)
	}
	truncatedAttr, ok := attrMap["requestBodyTruncated"]
	if !ok {
		t.Fatal("requestBodyTruncated attribute missing")
	}
	if !truncatedAttr.Bool() {
		t.Fatal("requestBodyTruncated attribute should be true")
	}
	sizeAttr, ok := attrMap["requestBodyBytes"]
	if !ok {
		t.Fatal("requestBodyBytes attribute missing")
	}
	if sizeAttr.Int64() != int64(len(body)) {
		t.Fatalf("requestBodyBytes = %d, want %d", sizeAttr.Int64(), len(body))
	}

	responsePreview, ok := attrMap["responseBody"]
	if !ok {
		t.Fatal("responseBody attribute missing")
	}
	if gotLen := len(responsePreview.String()); gotLen != bodyPreviewInlineLimit {
		t.Fatalf("responseBody preview length = %d, want %d", gotLen, bodyPreviewInlineLimit)
	}
	respTruncatedAttr, ok := attrMap["responseBodyTruncated"]
	if !ok {
		t.Fatal("responseBodyTruncated attribute missing")
	}
	if !respTruncatedAttr.Bool() {
		t.Fatal("responseBodyTruncated attribute should be true")
	}
	respSizeAttr, ok := attrMap["responseBodyBytes"]
	if !ok {
		t.Fatal("responseBodyBytes attribute missing")
	}
	if respSizeAttr.Int64() != int64(len(body)) {
		t.Fatalf("responseBodyBytes = %d, want %d", respSizeAttr.Int64(), len(body))
	}
}

func TestAppendBodyAttrsSmallPayload(t *testing.T) {
	cb := newCappedBuffer(64)
	if cb == nil {
		t.Fatal("expected capped buffer to be allocated")
	}
	if _, err := cb.Write([]byte("ok")); err != nil {
		t.Fatalf("failed to write buffer: %v", err)
	}

	attrs := appendBodyAttrs(nil, "body", cb)
	if len(attrs) != 2 {
		t.Fatalf("appendBodyAttrs returned %d attrs, want 2", len(attrs))
	}
	if attrs[0].Key != "body" || attrs[0].Value.String() != "ok" {
		t.Fatalf("first attr = (%q,%q), want (body,ok)", attrs[0].Key, attrs[0].Value.String())
	}
	if attrs[1].Key != "bodyBytes" || attrs[1].Value.Int64() != 2 {
		t.Fatalf("second attr = (%q,%d), want (bodyBytes,2)", attrs[1].Key, attrs[1].Value.Int64())
	}
}

func TestCappedBuffer(t *testing.T) {
	cb := newCappedBuffer(5)
	if cb == nil {
		t.Fatal("expected non-nil cappedBuffer for positive limit")
	}
	if n, err := cb.Write([]byte("hello")); err != nil || n != len("hello") {
		t.Fatalf("first write = (%d,%v), want (%d,nil)", n, err, len("hello"))
	}
	if cb.Truncated() {
		t.Fatal("buffer reports truncated after fit write")
	}
	if cb.String() != "hello" || cb.Len() != 5 {
		t.Fatalf("buffer contents %q len %d, want hello len 5", cb.String(), cb.Len())
	}

	if n, err := cb.Write([]byte(" world")); err != nil || n != len(" world") {
		t.Fatalf("second write = (%d,%v), want (%d,nil)", n, err, len(" world"))
	}
	if !cb.Truncated() {
		t.Fatal("expected truncated flag after overflow write")
	}
	if cb.String() != "hello" {
		t.Fatalf("buffer contents = %q, want hello", cb.String())
	}
	if cb.Len() != 5 {
		t.Fatalf("buffer len = %d, want 5", cb.Len())
	}

	if n, err := cb.Write([]byte("!")); err != nil || n != 1 {
		t.Fatalf("third write = (%d,%v), want (1,nil)", n, err)
	}
	if !cb.Truncated() {
		t.Fatal("expected truncated to remain true")
	}

	var nilBuf *cappedBuffer
	if n, err := nilBuf.Write([]byte("abc")); err != nil || n != len("abc") {
		t.Fatalf("nil buffer write = (%d,%v), want (%d,nil)", n, err, len("abc"))
	}
	if nilBuf.Truncated() || nilBuf.Len() != 0 || nilBuf.String() != "" {
		t.Fatal("nil buffer should behave like disabled capture")
	}

	if newCappedBuffer(0) != nil {
		t.Fatal("expected limit<=0 to return nil buffer")
	}

	// ensure String respects captured bytes even if underlying buffer mutated
	if _, err := cb.Write([]byte{}); err != nil {
		t.Fatalf("zero write returned error: %v", err)
	}
	if cb.String() != "hello" {
		t.Fatalf("buffer contents changed unexpectedly: %q", cb.String())
	}

	if ip := extractIP("[2001:db8::1]:443"); ip != "2001:db8::1" {
		t.Fatalf("extractIP IPv6 = %q, want 2001:db8::1", ip)
	}
	if ip := extractIP("198.51.100.20:80"); ip != "198.51.100.20" {
		t.Fatalf("extractIP IPv4 = %q, want 198.51.100.20", ip)
	}
	if ip := extractIP("invalid"); ip != "invalid" {
		t.Fatalf("extractIP invalid = %q, want original string", ip)
	}
}

func recordAttrsToMap(rec slog.Record) map[string]slog.Value {
	result := make(map[string]slog.Value)
	rec.Attrs(func(attr slog.Attr) bool {
		result[attr.Key] = attr.Value
		return true
	})
	return result
}
