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
	"context"
	"fmt"
	"net"
	stdhttp "net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/pjscruggs/slogcp"
)

// Transport returns an http.RoundTripper that injects trace context and derives
// a logger per outbound request.
func Transport(base stdhttp.RoundTripper, opts ...Option) stdhttp.RoundTripper {
	cfg := applyOptions(opts)
	if base == nil {
		base = stdhttp.DefaultTransport
	}

	projectID := strings.TrimSpace(cfg.projectID)
	if projectID == "" {
		projectID = strings.TrimSpace(slogcp.DetectRuntimeInfo().ProjectID)
	}

	return roundTripper{
		base:      base,
		cfg:       cfg,
		projectID: projectID,
	}
}

type roundTripper struct {
	base      stdhttp.RoundTripper
	cfg       *config
	projectID string
}

// RoundTrip instruments the outbound request, attaching context and forwarding to the base transport.
func (t roundTripper) RoundTrip(req *stdhttp.Request) (*stdhttp.Response, error) {
	if req == nil {
		resp, err := t.base.RoundTrip(req)
		if err != nil {
			return nil, fmt.Errorf("round trip nil request: %w", err)
		}
		return resp, nil
	}

	cfg := t.cfg
	ctx := req.Context()

	scope := newClientScope(req, time.Now(), cfg)

	traceAttrs, _ := slogcp.TraceAttributes(ctx, t.projectID)
	attrs := scope.loggerAttrs(cfg, traceAttrs)

	for _, enricher := range cfg.attrEnrichers {
		if enricher == nil {
			continue
		}
		if extra := enricher(req, scope); len(extra) > 0 {
			attrs = append(attrs, extra...)
		}
	}
	for _, transformer := range cfg.attrTransformers {
		if transformer == nil {
			continue
		}
		attrs = transformer(attrs, req, scope)
	}

	baseLogger := cfg.logger
	if baseLogger == nil {
		baseLogger = slogcp.Logger(ctx)
	}
	requestLogger := loggerWithAttrs(baseLogger, attrs)

	ctx = slogcp.ContextWithLogger(ctx, requestLogger)
	ctx = context.WithValue(ctx, requestScopeKey{}, scope)

	req = req.WithContext(ctx)

	t.injectTrace(ctx, req)

	resp, err := t.base.RoundTrip(req)
	elapsed := time.Since(scope.Start())

	if resp != nil {
		scope.finalize(resp.StatusCode, resp.ContentLength, elapsed)
		if err != nil {
			return resp, fmt.Errorf("round trip request: %w", err)
		}
		return resp, nil
	}

	scope.finalize(0, scope.ResponseSize(), elapsed)
	if err != nil {
		return nil, fmt.Errorf("round trip request: %w", err)
	}
	return nil, fmt.Errorf("round trip request: received no response and no error")
}

// injectTrace injects OpenTelemetry and optional legacy trace headers onto the request.
func (t roundTripper) injectTrace(ctx context.Context, req *stdhttp.Request) {
	if t.cfg != nil && !t.cfg.propagateTrace {
		return
	}

	propagator := t.cfg.propagators
	if propagator == nil {
		propagator = otel.GetTextMapPropagator()
	}
	if propagator != nil {
		propagator.Inject(ctx, propagation.HeaderCarrier(req.Header))
	}

	if !t.cfg.injectLegacyXCTC {
		return
	}

	if req.Header.Get(XCloudTraceContextHeader) != "" {
		return
	}

	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return
	}

	req.Header.Set(
		XCloudTraceContextHeader,
		slogcp.BuildXCloudTraceContext(sc.TraceID().String(), sc.SpanID().String(), sc.IsSampled()),
	)
}

// newClientScope builds a RequestScope describing the outbound HTTP request.
func newClientScope(req *stdhttp.Request, start time.Time, cfg *config) *RequestScope {
	scope := &RequestScope{
		start:       start,
		method:      req.Method,
		requestSize: req.ContentLength,
	}

	if req.URL != nil {
		scope.target = req.URL.Path
		scope.query = req.URL.RawQuery
		scope.scheme = req.URL.Scheme
		scope.host = req.URL.Host
	}
	scope.userAgent = req.Header.Get("User-Agent")
	if cfg.includeClientIP {
		scope.clientIP = outboundHost(req)
	}

	scope.status.Store(stdhttp.StatusOK)
	scope.latencyNS.Store(-1)
	return scope
}

// outboundHost extracts the host name from the outbound request for logging.
func outboundHost(req *stdhttp.Request) string {
	if req == nil {
		return ""
	}
	if req.URL != nil && req.URL.Host != "" {
		if host, _, err := net.SplitHostPort(req.URL.Host); err == nil {
			return host
		}
		return req.URL.Host
	}
	if req.Host != "" {
		if host, _, err := net.SplitHostPort(req.Host); err == nil {
			return host
		}
		return req.Host
	}
	return ""
}
