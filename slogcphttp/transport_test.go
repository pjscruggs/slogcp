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

package slogcphttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"

	"github.com/pjscruggs/slogcp"
)

// TestTransportRoundTripDerivesScope verifies RoundTrip attaches a scope and logger.
func TestTransportRoundTripDerivesScope(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{AddSource: false})
	baseLogger := slog.New(handler)

	resp := &http.Response{
		StatusCode:    http.StatusAccepted,
		Body:          io.NopCloser(strings.NewReader("ok")),
		ContentLength: 2,
	}
	stub := &stubRoundTripper{resp: resp}

	rt := Transport(stub, WithLogger(baseLogger), WithProjectID("proj-123"))

	req := httptest.NewRequest(http.MethodPost, "https://example.com/api", strings.NewReader("body"))
	req.Header.Set("User-Agent", "test-client")

	gotResp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip returned %v", err)
	}
	defer func() {
		if cerr := gotResp.Body.Close(); cerr != nil {
			t.Fatalf("gotResp.Body.Close() returned %v", cerr)
		}
	}()

	if stub.req == nil {
		t.Fatalf("base RoundTripper not invoked")
	}

	scope, ok := ScopeFromContext(stub.req.Context())
	if !ok {
		t.Fatalf("request scope missing from context")
	}
	if scope.Status() != http.StatusAccepted {
		t.Fatalf("scope.Status = %d, want %d", scope.Status(), http.StatusAccepted)
	}
	if scope.ResponseSize() != resp.ContentLength {
		t.Fatalf("scope.ResponseSize = %d, want %d", scope.ResponseSize(), resp.ContentLength)
	}

	if logger := slogcpLogger(stub.req.Context()); logger == nil {
		t.Fatalf("request logger missing from context")
	}
}

// TestTransportRoundTripHandlesNilRequest ensures nil requests reach the base transport.
func TestTransportRoundTripHandlesNilRequest(t *testing.T) {
	t.Parallel()

	errStub := &stubRoundTripper{err: errors.New("boom")}
	rt := Transport(errStub)

	if _, err := rt.RoundTrip(nil); !errors.Is(err, errStub.err) {
		t.Fatalf("RoundTrip error = %v, want boom", err)
	}
	if errStub.req != nil {
		t.Fatalf("expected nil request forwarded to base transport")
	}

	respStub := &stubRoundTripper{
		resp: &http.Response{
			StatusCode:    http.StatusAccepted,
			Body:          io.NopCloser(strings.NewReader("ok")),
			ContentLength: 2,
		},
	}
	rt = Transport(respStub)
	resp, err := rt.RoundTrip(nil)
	if err != nil {
		t.Fatalf("RoundTrip nil request returned %v", err)
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			t.Fatalf("resp.Body.Close() returned %v", cerr)
		}
	}()
	if respStub.req != nil {
		t.Fatalf("expected nil request forwarded to base transport for response stub")
	}
}

// TestTransportRoundTripRecordsFailure ensures finalize runs when the base transport returns no response.
func TestTransportRoundTripRecordsFailure(t *testing.T) {
	t.Parallel()

	errStub := &stubRoundTripper{err: errors.New("boom")}
	rt := Transport(errStub)

	req := httptest.NewRequest(http.MethodGet, "https://example.com", nil)

	if _, err := rt.RoundTrip(req); !errors.Is(err, errStub.err) {
		t.Fatalf("RoundTrip error = %v, want boom", err)
	}
	if errStub.req == nil {
		t.Fatalf("base RoundTripper was not invoked")
	}
	scope, ok := ScopeFromContext(errStub.req.Context())
	if !ok {
		t.Fatalf("request scope missing after failed RoundTrip")
	}
	if scope.Status() != http.StatusOK {
		t.Fatalf("scope.Status = %d, want %d", scope.Status(), http.StatusOK)
	}
	if lat, _ := scope.Latency(); lat < 0 {
		t.Fatalf("scope.Latency = %v, want >= 0", lat)
	}
}

// TestTransportRoundTripAppliesAttrHooks ensures attr enrichers and transformers run.
func TestTransportRoundTripAppliesAttrHooks(t *testing.T) {
	t.Parallel()

	var (
		sawEnricher    bool
		sawTransformer bool
	)

	rt := Transport(&stubRoundTripper{}, WithAttrEnricher(func(r *http.Request, scope *RequestScope) []slog.Attr {
		sawEnricher = true
		return []slog.Attr{slog.String("extra", "value")}
	}), WithAttrTransformer(func(attrs []slog.Attr, r *http.Request, scope *RequestScope) []slog.Attr {
		sawTransformer = true
		return attrs[:0]
	}))

	req := httptest.NewRequest(http.MethodGet, "https://example.com/widgets", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip returned %v", err)
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			t.Fatalf("resp.Body.Close() returned %v", cerr)
		}
	}()

	if !sawEnricher {
		t.Fatalf("attr enricher did not run")
	}
	if !sawTransformer {
		t.Fatalf("attr transformer did not run")
	}
}

// TestTransportSkipsNilHooks ensures nil attr hooks do not panic.
func TestTransportSkipsNilHooks(t *testing.T) {
	t.Parallel()

	var (
		sawEnricher    bool
		sawTransformer bool
	)

	rt := Transport(&stubRoundTripper{}, withRawAttrHooks(
		[]AttrEnricher{
			nil,
			// observeEnricher sets a flag so the test knows the enricher hook executed.
			func(r *http.Request, scope *RequestScope) []slog.Attr {
				sawEnricher = true
				return nil
			},
		},
		[]AttrTransformer{
			nil,
			// observeTransformer toggles a flag when the transformer hook is invoked.
			func(attrs []slog.Attr, r *http.Request, scope *RequestScope) []slog.Attr {
				sawTransformer = true
				return attrs
			},
		},
	))

	req := httptest.NewRequest(http.MethodGet, "https://example.com/nil-hooks", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip returned %v", err)
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			t.Fatalf("resp.Body.Close() returned %v", cerr)
		}
	}()

	if !sawEnricher {
		t.Fatalf("expected enricher to run")
	}
	if !sawTransformer {
		t.Fatalf("expected transformer to run")
	}
}

// TestTransportUsesContextLoggerFallback verifies the context logger is used when no base logger is set.
func TestTransportUsesContextLoggerFallback(t *testing.T) {
	t.Parallel()

	ctxLogger := slog.New(&spyHandler{name: "ctx"})
	stub := &stubRoundTripper{}
	rt := Transport(stub, func(cfg *config) { cfg.logger = nil })

	req := httptest.NewRequest(http.MethodGet, "https://example.com/logger", nil)
	req = req.WithContext(slogcp.ContextWithLogger(req.Context(), ctxLogger))

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip returned %v", err)
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			t.Fatalf("resp.Body.Close() returned %v", cerr)
		}
	}()

	logger := slogcp.Logger(stub.req.Context())
	handler, ok := logger.Handler().(*spyHandler)
	if !ok {
		t.Fatalf("handler type = %T, want *spyHandler", logger.Handler())
	}
	if handler.name != "ctx" {
		t.Fatalf("handler name = %q, want ctx", handler.name)
	}
}

// TestTransportRoundTripReturnsWrappedErrorWithResponse covers the branch where a response is returned alongside an error.
func TestTransportRoundTripReturnsWrappedErrorWithResponse(t *testing.T) {
	t.Parallel()

	resp := &http.Response{
		StatusCode:    http.StatusTeapot,
		Body:          io.NopCloser(strings.NewReader("err")),
		ContentLength: 3,
	}
	base := respErrRoundTripper{resp: resp, err: errors.New("base-fail")}
	rt := Transport(base)

	req := httptest.NewRequest(http.MethodGet, "https://example.com/with-error", nil)
	gotResp, err := rt.RoundTrip(req)
	if gotResp == nil || gotResp.StatusCode != http.StatusTeapot {
		t.Fatalf("RoundTrip response = %+v, want status %d", gotResp, http.StatusTeapot)
	}
	if err == nil || !errors.Is(err, base.err) {
		t.Fatalf("RoundTrip err = %v, want wrapped base-fail", err)
	}
}

// TestTransportRoundTripDetectsMissingResponse covers the no-response/no-error branch.
func TestTransportRoundTripDetectsMissingResponse(t *testing.T) {
	t.Parallel()

	rt := Transport(respErrRoundTripper{})
	req := httptest.NewRequest(http.MethodGet, "https://example.com/missing", nil)

	if _, err := rt.RoundTrip(req); err == nil || !strings.Contains(err.Error(), "received no response") {
		t.Fatalf("RoundTrip err = %v, want missing response error", err)
	}
}

// TestRoundTripperInjectTraceLegacy covers header injection scenarios.
func TestRoundTripperInjectTraceLegacy(t *testing.T) {
	t.Parallel()

	cfg := defaultConfig()
	cfg.injectLegacyXCTC = true
	rt := roundTripper{cfg: cfg}

	traceID, _ := trace.TraceIDFromHex("105445aa7843bc8bf206b12000100000")
	spanID, _ := trace.SpanIDFromHex("09158d8185d3c3af")
	spanCtx := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), spanCtx)

	t.Run("injects_when_missing", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "https://example.com", nil)
		rt.injectTrace(ctx, req)
		if req.Header.Get(XCloudTraceContextHeader) == "" {
			t.Fatalf("legacy header not injected")
		}
	})

	t.Run("skips_when_existing", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "https://example.com", nil)
		req.Header.Set(XCloudTraceContextHeader, "existing")
		rt.injectTrace(ctx, req)
		if req.Header.Get(XCloudTraceContextHeader) != "existing" {
			t.Fatalf("existing header should remain untouched")
		}
	})

	t.Run("skips_without_span", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "https://example.com", nil)
		rt.injectTrace(context.Background(), req)
		if req.Header.Get(XCloudTraceContextHeader) != "" {
			t.Fatalf("header should not be injected without span context")
		}
	})
}

// TestTransportSkipsTraceInjectionWhenDisabled verifies trace headers are not injected when propagation is disabled.
func TestTransportSkipsTraceInjectionWhenDisabled(t *testing.T) {
	t.Parallel()

	stub := &stubRoundTripper{}
	rt := Transport(
		stub,
		WithLegacyXCloudInjection(true),
		WithTracePropagation(false),
	)

	traceID, _ := trace.TraceIDFromHex("105445aa7843bc8bf206b12000100000")
	spanID, _ := trace.SpanIDFromHex("09158d8185d3c3af")
	spanCtx := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	})

	req := httptest.NewRequest(http.MethodGet, "https://example.com/api", nil).WithContext(trace.ContextWithSpanContext(context.Background(), spanCtx))
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip returned %v", err)
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			t.Fatalf("resp.Body.Close() returned %v", cerr)
		}
	}()

	if stub.req == nil {
		t.Fatalf("base RoundTripper not invoked")
	}
	if got := stub.req.Header.Get("traceparent"); got != "" {
		t.Fatalf("traceparent header injected unexpectedly: %q", got)
	}
	if got := stub.req.Header.Get(XCloudTraceContextHeader); got != "" {
		t.Fatalf("x-cloud-trace-context injected unexpectedly: %q", got)
	}
}

// TestTransportBranches covers default transport selection and project ID fallback.
func TestTransportBranches(t *testing.T) {
	t.Parallel()

	rt := Transport(nil, WithProjectID(" proj "), WithTracePropagation(false))
	typed := rt.(roundTripper)
	if typed.base == nil {
		t.Fatalf("expected default transport when base is nil")
	}
	if typed.projectID != "proj" {
		t.Fatalf("projectID trimming failed, got %q", typed.projectID)
	}

	rec := &recordingRoundTripper{}
	rt = Transport(rec, WithProjectID(""), WithLegacyXCloudInjection(true))
	typed = rt.(roundTripper)
	req := httptest.NewRequest(http.MethodGet, "http://example.com", nil).WithContext(context.Background())
	if _, err := typed.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip returned error: %v", err)
	}
	if rec.req == nil {
		t.Fatalf("base RoundTrip not invoked")
	}
}

// TestTransportInjectsTraceAndLogger ensures the transport adds trace headers and a logger.
func TestTransportInjectsTraceAndLogger(t *testing.T) {
	var buf bytes.Buffer
	baseLogger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{AddSource: false}))

	stub := &stubRoundTripper{
		resp: &http.Response{
			StatusCode: http.StatusAccepted,
			Body:       io.NopCloser(strings.NewReader("ok")),
		},
	}
	rt := Transport(
		stub,
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

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.example.com/v1/resource", http.NoBody)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("User-Agent", "test-client/1.0")

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("response code = %d", resp.StatusCode)
	}

	if stub.req == nil {
		t.Fatalf("captured request missing")
	}

	if got := stub.req.Header.Get("traceparent"); got == "" {
		t.Fatalf("traceparent header missing")
	}

	expectedXCTC := "105445aa7843bc8bf206b12000100000/654584908287820719;o=1"
	if got := stub.req.Header.Get(XCloudTraceContextHeader); got != expectedXCTC {
		t.Fatalf("x-cloud-trace-context = %q want %q", got, expectedXCTC)
	}

	// stub.req.Context() should have the logger
	logger := slogcp.Logger(stub.req.Context())
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
	if got := entry["server.address"]; got != "api.example.com" {
		t.Errorf("server.address = %v", got)
	}
	if _, ok := entry["network.peer.ip"]; ok {
		t.Errorf("network.peer.ip should be omitted for outbound requests")
	}
	if got := entry["logging.googleapis.com/trace"]; got != "projects/proj-123/traces/105445aa7843bc8bf206b12000100000" {
		t.Errorf("trace = %v", got)
	}
}

// TestOutboundHost exercises the host resolution helper.
func TestOutboundHost(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		req  *http.Request
		want string
	}{
		{
			name: "url_host_with_port",
			req:  httptest.NewRequest(http.MethodGet, "https://example.com:443/data", nil),
			want: "example.com",
		},
		{
			name: "host_header_only",
			req: func() *http.Request {
				req := new(http.Request)
				req.Host = "widgets.internal:8080"
				return req
			}(),
			want: "widgets.internal",
		},
		{
			name: "host_header_no_port",
			req: func() *http.Request {
				req := new(http.Request)
				req.Host = "api.internal"
				return req
			}(),
			want: "api.internal",
		},
		{
			name: "empty_request",
			req:  nil,
			want: "",
		},
		{
			name: "no_host_information",
			req:  &http.Request{URL: &url.URL{Path: "/only-path"}},
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := outboundHost(tc.req); got != tc.want {
				t.Fatalf("outboundHost() = %q, want %q", got, tc.want)
			}
		})
	}
}

type recordingRoundTripper struct {
	req *http.Request
}

// RoundTrip records the last request for assertions.
func (r *recordingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	r.req = req
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: req}, nil
}

type stubRoundTripper struct {
	req  *http.Request
	resp *http.Response
	err  error
}

// RoundTrip captures the request and returns the canned response.
func (s *stubRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	s.req = req
	if s.err != nil {
		return nil, s.err
	}
	if s.resp == nil {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok")), ContentLength: 2}, nil
	}
	s.resp.Request = req
	return s.resp, nil
}

// slogcpLogger fetches the slogcp logger from context for assertions.
func slogcpLogger(ctx context.Context) *slog.Logger {
	if ctx == nil {
		return nil
	}
	return slogcp.Logger(ctx)
}

type respErrRoundTripper struct {
	resp *http.Response
	err  error
}

// RoundTrip returns the canned response/error for coverage of error branches.
func (r respErrRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return r.resp, r.err
}

type spyHandler struct {
	name string
}

// Enabled reports true so tests can exercise downstream logic without filtering.
func (s *spyHandler) Enabled(context.Context, slog.Level) bool { return true }

// Handle discards records to satisfy slog.Handler without producing output.
func (s *spyHandler) Handle(context.Context, slog.Record) error { return nil }

// WithAttrs clones the handler name to mimic slog.Handler semantics for assertions.
func (s *spyHandler) WithAttrs([]slog.Attr) slog.Handler { return &spyHandler{name: s.name} }

// WithGroup returns a similarly configured spy handler for nested groups.
func (s *spyHandler) WithGroup(string) slog.Handler { return &spyHandler{name: s.name} }
