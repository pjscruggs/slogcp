package http

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pjscruggs/slogcp"
	"go.opentelemetry.io/otel/trace"
)

// TestTransportRoundTripDerivesScope verifies RoundTrip attaches a scope and logger.
func TestTransportRoundTripDerivesScope(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{AddSource: false})
	baseLogger := slog.New(handler)

	resp := &stdhttp.Response{
		StatusCode:    stdhttp.StatusAccepted,
		Body:          io.NopCloser(strings.NewReader("ok")),
		ContentLength: 2,
	}
	stub := &stubRoundTripper{resp: resp}

	rt := Transport(stub, WithLogger(baseLogger), WithProjectID("proj-123"))

	req := httptest.NewRequest(stdhttp.MethodPost, "https://example.com/api", strings.NewReader("body"))
	req.Header.Set("User-Agent", "test-client")

	gotResp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip returned %v", err)
	}
	defer gotResp.Body.Close()

	if stub.req == nil {
		t.Fatalf("base RoundTripper not invoked")
	}

	scope, ok := ScopeFromContext(stub.req.Context())
	if !ok {
		t.Fatalf("request scope missing from context")
	}
	if scope.Status() != stdhttp.StatusAccepted {
		t.Fatalf("scope.Status = %d, want %d", scope.Status(), stdhttp.StatusAccepted)
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
}

// TestTransportRoundTripRecordsFailure ensures finalize runs when the base transport returns no response.
func TestTransportRoundTripRecordsFailure(t *testing.T) {
	t.Parallel()

	errStub := &stubRoundTripper{err: errors.New("boom")}
	rt := Transport(errStub)

	req := httptest.NewRequest(stdhttp.MethodGet, "https://example.com", nil)

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
	if scope.Status() != stdhttp.StatusOK {
		t.Fatalf("scope.Status = %d, want %d", scope.Status(), stdhttp.StatusOK)
	}
	if scope.Latency() < 0 {
		t.Fatalf("scope.Latency = %v, want >= 0", scope.Latency())
	}
}

// TestTransportRoundTripAppliesAttrHooks ensures attr enrichers and transformers run.
func TestTransportRoundTripAppliesAttrHooks(t *testing.T) {
	t.Parallel()

	var (
		sawEnricher    bool
		sawTransformer bool
	)

	rt := Transport(&stubRoundTripper{}, WithAttrEnricher(func(r *stdhttp.Request, scope *RequestScope) []slog.Attr {
		sawEnricher = true
		return []slog.Attr{slog.String("extra", "value")}
	}), WithAttrTransformer(func(attrs []slog.Attr, r *stdhttp.Request, scope *RequestScope) []slog.Attr {
		sawTransformer = true
		return attrs[:0]
	}))

	req := httptest.NewRequest(stdhttp.MethodGet, "https://example.com/widgets", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip returned %v", err)
	}
	defer resp.Body.Close()

	if !sawEnricher {
		t.Fatalf("attr enricher did not run")
	}
	if !sawTransformer {
		t.Fatalf("attr transformer did not run")
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
		req := httptest.NewRequest(stdhttp.MethodGet, "https://example.com", nil)
		rt.injectTrace(ctx, req)
		if req.Header.Get(XCloudTraceContextHeader) == "" {
			t.Fatalf("legacy header not injected")
		}
	})

	t.Run("skips_when_existing", func(t *testing.T) {
		req := httptest.NewRequest(stdhttp.MethodGet, "https://example.com", nil)
		req.Header.Set(XCloudTraceContextHeader, "existing")
		rt.injectTrace(ctx, req)
		if req.Header.Get(XCloudTraceContextHeader) != "existing" {
			t.Fatalf("existing header should remain untouched")
		}
	})

	t.Run("skips_without_span", func(t *testing.T) {
		req := httptest.NewRequest(stdhttp.MethodGet, "https://example.com", nil)
		rt.injectTrace(context.Background(), req)
		if req.Header.Get(XCloudTraceContextHeader) != "" {
			t.Fatalf("header should not be injected without span context")
		}
	})
}

// TestOutboundHost exercises the host resolution helper.
func TestOutboundHost(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		req  *stdhttp.Request
		want string
	}{
		{
			name: "url_host_with_port",
			req:  httptest.NewRequest(stdhttp.MethodGet, "https://example.com:443/data", nil),
			want: "example.com",
		},
		{
			name: "host_header_only",
			req: func() *stdhttp.Request {
				req := new(stdhttp.Request)
				req.Host = "widgets.internal:8080"
				return req
			}(),
			want: "widgets.internal",
		},
		{
			name: "host_header_no_port",
			req: func() *stdhttp.Request {
				req := new(stdhttp.Request)
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
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := outboundHost(tc.req); got != tc.want {
				t.Fatalf("outboundHost() = %q, want %q", got, tc.want)
			}
		})
	}
}

type stubRoundTripper struct {
	req  *stdhttp.Request
	resp *stdhttp.Response
	err  error
}

// RoundTrip captures the request and returns the canned response.
func (s *stubRoundTripper) RoundTrip(req *stdhttp.Request) (*stdhttp.Response, error) {
	s.req = req
	if s.err != nil {
		return nil, s.err
	}
	if s.resp == nil {
		return &stdhttp.Response{StatusCode: stdhttp.StatusOK, Body: io.NopCloser(strings.NewReader("ok")), ContentLength: 2}, nil
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
