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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pjscruggs/slogcp"
	"go.opentelemetry.io/otel/propagation"
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

// TestWrapResponseWriterPreservesOptionalInterfaces ensures wrapResponseWriter retains
// optional HTTP interfaces such as Flusher, Hijacker, Pusher, CloseNotifier, and ReaderFrom.
func TestWrapResponseWriterPreservesOptionalInterfaces(t *testing.T) {
	t.Parallel()

	base := newOptionalResponseWriter()
	req := httptest.NewRequest(stdhttp.MethodGet, "https://example.com/stream", stdhttp.NoBody)
	cfg := defaultConfig()
	scope := newRequestScope(req, time.Now(), cfg)

	wrapped, recorder := wrapResponseWriter(base, scope)

	flusher, ok := wrapped.(stdhttp.Flusher)
	if !ok {
		t.Fatalf("wrapped writer missing stdhttp.Flusher")
	}
	flusher.Flush()
	if base.flushCount != 1 {
		t.Fatalf("Flush not forwarded, got %d calls", base.flushCount)
	}

	hijacker, ok := wrapped.(stdhttp.Hijacker)
	if !ok {
		t.Fatalf("wrapped writer missing stdhttp.Hijacker")
	}
	hConn, hBuf, err := hijacker.Hijack()
	if err != nil {
		t.Fatalf("Hijack error: %v", err)
	}
	if hConn != base.hijackConn {
		t.Fatalf("Hijack returned unexpected connection")
	}
	if hBuf != base.hijackRW {
		t.Fatalf("Hijack returned unexpected bufio.ReadWriter")
	}

	pusher, ok := wrapped.(stdhttp.Pusher)
	if !ok {
		t.Fatalf("wrapped writer missing stdhttp.Pusher")
	}
	if err := pusher.Push("/events", &stdhttp.PushOptions{Method: stdhttp.MethodGet}); err != nil {
		t.Fatalf("Push error: %v", err)
	}
	if len(base.pushedTargets) != 1 || base.pushedTargets[0] != "/events" {
		t.Fatalf("Push target not recorded, got %v", base.pushedTargets)
	}

	closeNotifier, ok := wrapped.(interface{ CloseNotify() <-chan bool })
	if !ok {
		t.Fatalf("wrapped writer missing CloseNotify support")
	}
	if ch := closeNotifier.CloseNotify(); ch != base.closeCh {
		t.Fatalf("CloseNotify returned unexpected channel")
	}

	// Ensure recorder reference is returned for completeness.
	if recorder == nil {
		t.Fatalf("recorder not returned")
	}
}

// TestWrapResponseWriterReadFromAccounting ensures ReadFrom instrumentation tracks bytes.
func TestWrapResponseWriterReadFromAccounting(t *testing.T) {
	t.Parallel()

	base := &readerFromResponseWriter{header: make(stdhttp.Header)}
	req := httptest.NewRequest(stdhttp.MethodGet, "https://example.com/download", stdhttp.NoBody)
	scope := newRequestScope(req, time.Now(), defaultConfig())

	wrapped, recorder := wrapResponseWriter(base, scope)

	readerFrom, ok := wrapped.(io.ReaderFrom)
	if !ok {
		t.Fatalf("wrapped writer missing io.ReaderFrom")
	}
	n, err := readerFrom.ReadFrom(strings.NewReader("payload"))
	if err != nil {
		t.Fatalf("ReadFrom error: %v", err)
	}
	if n != int64(len("payload")) {
		t.Fatalf("ReadFrom bytes = %d, want %d", n, len("payload"))
	}
	if base.readFromBytes != n {
		t.Fatalf("underlying writer did not observe ReadFrom bytes")
	}
	if recorder.BytesWritten() != n {
		t.Fatalf("recorder.BytesWritten = %d, want %d", recorder.BytesWritten(), n)
	}
	if scope.ResponseSize() != n {
		t.Fatalf("scope.ResponseSize = %d, want %d", scope.ResponseSize(), n)
	}
}

// TestRequestScopeFinalizeClampsValues ensures status, bytes, and latency handling cover edge cases.
func TestRequestScopeFinalizeClampsValues(t *testing.T) {
	t.Parallel()

	cfg := defaultConfig()
	req := httptest.NewRequest(stdhttp.MethodGet, "https://example.com/items?id=42", strings.NewReader("body"))
	req.RemoteAddr = "203.0.113.5:443"

	scope := newRequestScope(req, time.Now().Add(-10*time.Millisecond), cfg)
	if scope.Status() != stdhttp.StatusOK {
		t.Fatalf("default status = %d, want %d", scope.Status(), stdhttp.StatusOK)
	}

	scope.setStatus(0)
	if scope.Status() != stdhttp.StatusOK {
		t.Fatalf("zero status fallback = %d, want %d", scope.Status(), stdhttp.StatusOK)
	}

	scope.setStatus(stdhttp.StatusAccepted)
	if scope.Status() != stdhttp.StatusAccepted {
		t.Fatalf("setStatus = %d, want %d", scope.Status(), stdhttp.StatusAccepted)
	}

	scope.addResponseBytes(-1)
	if scope.ResponseSize() != 0 {
		t.Fatalf("negative bytes mutated size = %d", scope.ResponseSize())
	}
	scope.addResponseBytes(128)
	if scope.ResponseSize() != 128 {
		t.Fatalf("ResponseSize = %d, want 128", scope.ResponseSize())
	}

	scope.finalize(stdhttp.StatusGatewayTimeout, 256, 25*time.Millisecond)
	if scope.Status() != stdhttp.StatusGatewayTimeout {
		t.Fatalf("finalize status = %d, want %d", scope.Status(), stdhttp.StatusGatewayTimeout)
	}
	if scope.ResponseSize() != 256 {
		t.Fatalf("finalize ResponseSize = %d, want 256", scope.ResponseSize())
	}
	if got := scope.Latency(); got != 25*time.Millisecond {
		t.Fatalf("Latency = %v, want 25ms", got)
	}

	scope.finalize(0, -1, -time.Millisecond)
	if scope.Status() != stdhttp.StatusOK {
		t.Fatalf("finalize fallback status = %d, want %d", scope.Status(), stdhttp.StatusOK)
	}
	if scope.ResponseSize() != 256 {
		t.Fatalf("ResponseSize should remain unchanged, got %d", scope.ResponseSize())
	}
	if got := scope.latencyNS.Load(); got != 0 {
		t.Fatalf("expected latencyNS clamp to 0, got %d", got)
	}
}

// TestScopeFromContextNil verifies nil and empty contexts return no scope.
func TestScopeFromContextNil(t *testing.T) {
	t.Parallel()

	if scope, ok := ScopeFromContext(nil); scope != nil || ok {
		t.Fatalf("ScopeFromContext(nil) = (%v,%v), want (nil,false)", scope, ok)
	}
	if scope, ok := ScopeFromContext(context.Background()); scope != nil || ok {
		t.Fatalf("ScopeFromContext(empty) = (%v,%v), want (nil,false)", scope, ok)
	}
}

// TestResponseRecorderWriteAndStatus exercises Write and Status bookkeeping.
func TestResponseRecorderWriteAndStatus(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(stdhttp.MethodGet, "https://example.com", stdhttp.NoBody)
	scope := newRequestScope(req, time.Now(), defaultConfig())
	base := httptest.NewRecorder()

	wrapped, recorder := wrapResponseWriter(base, scope)
	if recorder.Status() != stdhttp.StatusOK {
		t.Fatalf("initial Status() = %d, want %d", recorder.Status(), stdhttp.StatusOK)
	}
	if _, err := wrapped.Write([]byte("payload")); err != nil {
		t.Fatalf("Write returned %v", err)
	}
	if recorder.Status() != stdhttp.StatusOK {
		t.Fatalf("Status after write = %d, want %d", recorder.Status(), stdhttp.StatusOK)
	}
	if recorder.BytesWritten() != int64(len("payload")) {
		t.Fatalf("BytesWritten = %d, want %d", recorder.BytesWritten(), len("payload"))
	}
	if scope.ResponseSize() != int64(len("payload")) {
		t.Fatalf("scope.ResponseSize = %d, want %d", scope.ResponseSize(), len("payload"))
	}

	recorder.WriteHeader(stdhttp.StatusTeapot)
	if recorder.Status() != stdhttp.StatusTeapot {
		t.Fatalf("Status = %d, want %d", recorder.Status(), stdhttp.StatusTeapot)
	}
}

// TestResponseRecorderReadFromFallback ensures io.Copy path accounts for bytes.
func TestResponseRecorderReadFromFallback(t *testing.T) {
	t.Parallel()

	scope := newRequestScope(httptest.NewRequest(stdhttp.MethodGet, "https://example.com", nil), time.Now(), defaultConfig())
	base := &minimalResponseWriter{header: make(stdhttp.Header)}

	wrapped, recorder := wrapResponseWriter(base, scope)
	readerFrom, ok := wrapped.(io.ReaderFrom)
	if !ok {
		t.Fatalf("wrap response writer did not expose ReaderFrom")
	}

	n, err := readerFrom.ReadFrom(strings.NewReader("chunks"))
	if err != nil {
		t.Fatalf("ReadFrom fallback returned %v", err)
	}
	if n != int64(len("chunks")) {
		t.Fatalf("ReadFrom n = %d, want %d", n, len("chunks"))
	}
	if scope.ResponseSize() != n {
		t.Fatalf("scope.ResponseSize = %d, want %d", scope.ResponseSize(), n)
	}
	if recorder.BytesWritten() != n {
		t.Fatalf("BytesWritten = %d, want %d", recorder.BytesWritten(), n)
	}
}

// TestResponseRecorderReadFromDelegates ensures ReaderFrom on the base writer is honoured.
func TestResponseRecorderReadFromDelegates(t *testing.T) {
	t.Parallel()

	scope := newRequestScope(httptest.NewRequest(stdhttp.MethodGet, "https://example.com", nil), time.Now(), defaultConfig())
	base := &readerFromResponseWriter{header: make(stdhttp.Header)}

	wrapped, recorder := wrapResponseWriter(base, scope)
	readerFrom, ok := wrapped.(io.ReaderFrom)
	if !ok {
		t.Fatalf("wrapped writer did not expose ReaderFrom")
	}

	n, err := readerFrom.ReadFrom(strings.NewReader("upstream-data"))
	if err != nil {
		t.Fatalf("ReadFrom returned %v", err)
	}
	if n != base.readFromBytes {
		t.Fatalf("base readFromBytes = %d, want %d", base.readFromBytes, n)
	}
	if recorder.BytesWritten() != n {
		t.Fatalf("BytesWritten = %d, want %d", recorder.BytesWritten(), n)
	}
	if scope.ResponseSize() != n {
		t.Fatalf("scope.ResponseSize = %d, want %d", scope.ResponseSize(), n)
	}
}

// TestResponseRecorderOptionalInterfacesFallback covers ErrNotSupported paths.
func TestResponseRecorderOptionalInterfacesFallback(t *testing.T) {
	t.Parallel()

	scope := newRequestScope(nil, time.Now(), defaultConfig())
	base := &minimalResponseWriter{header: make(stdhttp.Header)}
	_, recorder := wrapResponseWriter(base, scope)

	recorder.Flush()
	if _, _, err := recorder.Hijack(); !errors.Is(err, stdhttp.ErrNotSupported) {
		t.Fatalf("Hijack error = %v, want ErrNotSupported", err)
	}
	if err := recorder.Push("/events", nil); !errors.Is(err, stdhttp.ErrNotSupported) {
		t.Fatalf("Push error = %v, want ErrNotSupported", err)
	}
	if ch := recorder.CloseNotify(); ch != nil {
		t.Fatalf("CloseNotify = %v, want nil", ch)
	}
}

// TestResponseRecorderOptionalInterfacesForwarding verifies optional behaviours delegate to the wrapped writer.
func TestResponseRecorderOptionalInterfacesForwarding(t *testing.T) {
	t.Parallel()

	scope := newRequestScope(nil, time.Now(), defaultConfig())
	base := newOptionalResponseWriter()
	_, recorder := wrapResponseWriter(base, scope)

	recorder.Flush()
	if base.flushCount != 1 {
		t.Fatalf("Flush count = %d, want 1", base.flushCount)
	}

	conn, rw, err := recorder.Hijack()
	if err != nil {
		t.Fatalf("Hijack returned %v", err)
	}
	if conn != base.hijackConn || rw != base.hijackRW {
		t.Fatalf("Hijack returned unexpected connection pair")
	}

	if err := recorder.Push("/events", nil); err != nil {
		t.Fatalf("Push returned %v", err)
	}
	if len(base.pushedTargets) != 1 || base.pushedTargets[0] != "/events" {
		t.Fatalf("pushedTargets = %v, want [/events]", base.pushedTargets)
	}

	ch := recorder.CloseNotify()
	if ch == nil {
		t.Fatalf("CloseNotify returned nil channel")
	}

	base.closeCh <- true
	select {
	case got := <-ch:
		if !got {
			t.Fatalf("CloseNotify value = %v, want true", got)
		}
	case <-time.After(time.Second):
		t.Fatalf("CloseNotify did not forward values")
	}
}

// TestExtractIPVariants validates IP parsing for several address formats.
func TestExtractIPVariants(t *testing.T) {
	t.Parallel()

	tests := []struct {
		addr string
		want string
	}{
		{addr: "198.51.100.1:443", want: "198.51.100.1"},
		{addr: "[2001:db8::2]:8443", want: "2001:db8::2"},
		{addr: "2001:db8::1:8080", want: "2001:db8::1:8080"},
		{addr: "", want: ""},
		{addr: "example.com:80", want: "example.com"},
		{addr: "example.com", want: "example.com"},
	}

	for _, tt := range tests {
		if got := extractIP(tt.addr); got != tt.want {
			t.Fatalf("extractIP(%q) = %q, want %q", tt.addr, got, tt.want)
		}
	}
}

// TestLoggerWithAttrs ensures nil base loggers fall back to slog.Default().
func TestLoggerWithAttrs(t *testing.T) {
	defaultLogger := slog.Default()

	if got := loggerWithAttrs(nil, nil); got != defaultLogger {
		t.Fatalf("loggerWithAttrs(nil,nil) != slog.Default()")
	}

	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{AddSource: false})
	base := slog.New(handler)

	logger := loggerWithAttrs(base, []slog.Attr{slog.String("k", "v")})
	logger.Info("test")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected single line output, got %d", len(lines))
	}
	var entry map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("unmarshal log: %v", err)
	}
	if got := entry["k"]; got != "v" {
		t.Fatalf("attribute missing, got %v", got)
	}

	var defaultBuf bytes.Buffer
	customDefault := slog.New(slog.NewJSONHandler(&defaultBuf, &slog.HandlerOptions{AddSource: false}))
	prevDefault := slog.Default()
	slog.SetDefault(customDefault)
	defer slog.SetDefault(prevDefault)

	loggerWithAttrs(nil, []slog.Attr{slog.String("extra", "value")}).
		Info("using default")

	if !strings.Contains(defaultBuf.String(), `"extra":"value"`) {
		t.Fatalf("default logger output missing attribute: %s", defaultBuf.String())
	}
}

// TestEnsureSpanContextVariants covers the propagation fallbacks used by the middleware.
func TestEnsureSpanContextVariants(t *testing.T) {
	traceID, _ := trace.TraceIDFromHex("105445aa7843bc8bf206b12000100000")
	spanID, _ := trace.SpanIDFromHex("09158d8185d3c3af")

	t.Run("existing", func(t *testing.T) {
		ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
			TraceID:    traceID,
			SpanID:     spanID,
			TraceFlags: trace.FlagsSampled,
		}))
		req := httptest.NewRequest(stdhttp.MethodGet, "https://example.com", nil)

		gotCtx, sc := ensureSpanContext(ctx, req, defaultConfig())
		if gotCtx != ctx {
			t.Fatalf("ensureSpanContext returned new context for existing span")
		}
		if sc.TraceID() != traceID {
			t.Fatalf("TraceID = %s, want %s", sc.TraceID(), traceID)
		}
	})

	t.Run("propagator", func(t *testing.T) {
		cfg := defaultConfig()
		cfg.propagators = propagation.TraceContext{}

		req := httptest.NewRequest(stdhttp.MethodGet, "https://example.com", nil)
		carrier := propagation.HeaderCarrier(req.Header)

		src := trace.ContextWithRemoteSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
			TraceID:    traceID,
			SpanID:     spanID,
			TraceFlags: trace.FlagsSampled,
			Remote:     true,
		}))
		cfg.propagators.Inject(src, carrier)

		_, sc := ensureSpanContext(context.Background(), req, cfg)
		if sc.TraceID() != traceID || sc.SpanID() != spanID {
			t.Fatalf("propagator extraction failed, got %v", sc)
		}
	})

	t.Run("xcloud", func(t *testing.T) {
		req := httptest.NewRequest(stdhttp.MethodGet, "https://example.com", nil)
		req.Header.Set(XCloudTraceContextHeader, traceID.String()+"/33;o=1")

		_, sc := ensureSpanContext(context.Background(), req, defaultConfig())
		if sc.TraceID() != traceID {
			t.Fatalf("xcloud extraction failed, got %v", sc)
		}
	})

	t.Run("absent", func(t *testing.T) {
		req := httptest.NewRequest(stdhttp.MethodGet, "https://example.com", nil)
		if _, sc := ensureSpanContext(context.Background(), req, defaultConfig()); sc.IsValid() {
			t.Fatalf("unexpected span context detected: %v", sc)
		}
	})
}

// TestResponseRecorderStatusCoversDefaults validates Status() behavior with and without explicit writes.
func TestResponseRecorderStatusCoversDefaults(t *testing.T) {
	t.Parallel()

	var rr responseRecorder
	if got := rr.Status(); got != stdhttp.StatusOK {
		t.Fatalf("Status() = %d, want %d", got, stdhttp.StatusOK)
	}
	rr.status = stdhttp.StatusCreated
	if got := rr.Status(); got != stdhttp.StatusCreated {
		t.Fatalf("Status() = %d, want %d", got, stdhttp.StatusCreated)
	}
}

// TestRequestScopeStatusDefaults verifies the atomic status fallback behavior.
func TestRequestScopeStatusDefaults(t *testing.T) {
	t.Parallel()

	var scope RequestScope
	if got := scope.Status(); got != stdhttp.StatusOK {
		t.Fatalf("Status() = %d, want %d", got, stdhttp.StatusOK)
	}
	scope.setStatus(stdhttp.StatusBadRequest)
	if got := scope.Status(); got != stdhttp.StatusBadRequest {
		t.Fatalf("Status() = %d, want %d", got, stdhttp.StatusBadRequest)
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

// optionalResponseWriter implements every optional http.ResponseWriter interface for testing.
type optionalResponseWriter struct {
	header        stdhttp.Header
	status        int
	flushCount    int
	pushedTargets []string
	closeCh       chan bool
	hijackConn    net.Conn
	hijackRW      *bufio.ReadWriter
}

// newOptionalResponseWriter constructs a ResponseWriter implementing every optional interface.
func newOptionalResponseWriter() *optionalResponseWriter {
	return &optionalResponseWriter{
		header:     make(stdhttp.Header),
		closeCh:    make(chan bool, 1),
		hijackConn: nopConn{},
		hijackRW:   bufio.NewReadWriter(bufio.NewReader(strings.NewReader("")), bufio.NewWriter(io.Discard)),
	}
}

// Header implements http.ResponseWriter.
func (o *optionalResponseWriter) Header() stdhttp.Header { return o.header }

// Write reports that len(p) bytes were written.
func (o *optionalResponseWriter) Write(p []byte) (int, error) {
	return len(p), nil
}

// WriteHeader records the outgoing status code.
func (o *optionalResponseWriter) WriteHeader(status int) {
	o.status = status
}

// Flush updates the counter for verification.
func (o *optionalResponseWriter) Flush() {
	o.flushCount++
}

// Hijack exposes the canned connection/buffer pair.
func (o *optionalResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return o.hijackConn, o.hijackRW, nil
}

// Push records the HTTP/2 push target for assertions.
func (o *optionalResponseWriter) Push(target string, _ *stdhttp.PushOptions) error {
	o.pushedTargets = append(o.pushedTargets, target)
	return nil
}

// CloseNotify returns the pre-wired notification channel.
func (o *optionalResponseWriter) CloseNotify() <-chan bool {
	return o.closeCh
}

// nopConn is a minimal net.Conn implementation for hijack testing.
type nopConn struct{}

// Read implements net.Conn by returning EOF.
func (nopConn) Read([]byte) (int, error) { return 0, io.EOF }

// Write implements net.Conn by discarding bytes.
func (nopConn) Write([]byte) (int, error) { return 0, io.EOF }

// Close implements net.Conn with a no-op.
func (nopConn) Close() error { return nil }

// LocalAddr returns a fake local endpoint.
func (nopConn) LocalAddr() net.Addr { return nopAddr("local") }

// RemoteAddr returns a fake remote endpoint.
func (nopConn) RemoteAddr() net.Addr { return nopAddr("remote") }

// SetDeadline satisfies net.Conn without enforcing deadlines.
func (nopConn) SetDeadline(time.Time) error { return nil }

// SetReadDeadline satisfies net.Conn without enforcing deadlines.
func (nopConn) SetReadDeadline(time.Time) error { return nil }

// SetWriteDeadline satisfies net.Conn without enforcing deadlines.
func (nopConn) SetWriteDeadline(time.Time) error { return nil }

type nopAddr string

// Network reports the placeholder transport name.
func (a nopAddr) Network() string { return "nop" }

// String returns the printable address.
func (a nopAddr) String() string { return string(a) }

type readerFromResponseWriter struct {
	header        stdhttp.Header
	readFromBytes int64
}

// Header implements http.ResponseWriter.
func (r *readerFromResponseWriter) Header() stdhttp.Header { return r.header }

// Write reports the number of bytes provided.
func (r *readerFromResponseWriter) Write(p []byte) (int, error) { return len(p), nil }

// WriteHeader is a stub for interface compliance.
func (r *readerFromResponseWriter) WriteHeader(int) {}

// ReadFrom copies from the reader into io.Discard and records the byte count.
func (r *readerFromResponseWriter) ReadFrom(src io.Reader) (int64, error) {
	n, err := io.Copy(io.Discard, src)
	r.readFromBytes += n
	return n, err
}

// minimalResponseWriter implements only the base ResponseWriter for fallback tests.
type minimalResponseWriter struct {
	header stdhttp.Header
}

// Header implements http.ResponseWriter.
func (m *minimalResponseWriter) Header() stdhttp.Header { return m.header }

// Write writes len(p) bytes without exposing optional interfaces.
func (m *minimalResponseWriter) Write(p []byte) (int, error) { return len(p), nil }

// WriteHeader records the status code but performs no IO.
func (m *minimalResponseWriter) WriteHeader(int) {}
