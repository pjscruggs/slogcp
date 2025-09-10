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

// Package http provides an outbound RoundTripper that propagates trace context
// (W3C traceparent and Google X-Cloud-Trace-Context) for better log/trace
// correlation on GCP.
package http

import (
	"encoding/binary"
	"fmt"
	stdhttp "net/http"
	"strconv"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// TraceRoundTripperOption configures the behavior of NewTraceRoundTripper.
type TraceRoundTripperOption func(*traceRTConfig)

type traceRTConfig struct {
	injectTraceparent bool
	injectXCloud      bool
	skip              func(*stdhttp.Request) bool
}

// WithInjectTraceparent enables/disables W3C traceparent injection (default: true).
func WithInjectTraceparent(on bool) TraceRoundTripperOption {
	return func(c *traceRTConfig) { c.injectTraceparent = on }
}

// WithInjectXCloud enables/disables X-Cloud-Trace-Context injection (default: true).
func WithInjectXCloud(on bool) TraceRoundTripperOption {
	return func(c *traceRTConfig) { c.injectXCloud = on }
}

// WithSkip sets a predicate to skip propagation for specific requests
// (e.g., third-party hosts). If the function returns true, no headers are injected.
func WithSkip(f func(*stdhttp.Request) bool) TraceRoundTripperOption {
	return func(c *traceRTConfig) { c.skip = f }
}

// NewTraceRoundTripper wraps base (or http.DefaultTransport if nil) and injects
// trace headers derived from req.Context(). Existing "traceparent" or
// "X-Cloud-Trace-Context" headers are left untouched.
//
// Behavior:
//   - W3C traceparent is injected via the global OTel propagator.
//   - X-Cloud-Trace-Context is synthesized from the same span context
//     (span id converted to decimal; o=1 if sampled, else o=0).
//   - Both injections are opt-in by options but enabled by default.
//
// This transport does not create spans; it only propagates context so that
// downstream services can correlate logs/traces. To record spans, use OTel
// instrumentation in addition to (or instead of) this transport.
func NewTraceRoundTripper(base stdhttp.RoundTripper, opts ...TraceRoundTripperOption) stdhttp.RoundTripper {
	cfg := traceRTConfig{
		injectTraceparent: true,
		injectXCloud:      true,
	}
	for _, o := range opts {
		o(&cfg)
	}
	if base == nil {
		base = stdhttp.DefaultTransport
	}
	return &traceRoundTripper{base: base, cfg: cfg}
}

type traceRoundTripper struct {
	base stdhttp.RoundTripper
	cfg  traceRTConfig
}

// RoundTrip implements http.RoundTripper. It conditionally injects trace context
// headers ("traceparent" and "X-Cloud-Trace-Context") into req based on the
// current span context and configured options, then delegates to the wrapped transport.
func (t *traceRoundTripper) RoundTrip(req *stdhttp.Request) (*stdhttp.Response, error) {
	if t.cfg.skip != nil && t.cfg.skip(req) {
		return t.base.RoundTrip(req)
	}

	sc := trace.SpanContextFromContext(req.Context())
	if sc.IsValid() {
		// Inject W3C only if not already present.
		if t.cfg.injectTraceparent && req.Header.Get("traceparent") == "" {
			otel.GetTextMapPropagator().Inject(req.Context(), propagation.HeaderCarrier(req.Header))
		}

		// Inject X-Cloud-Trace-Context only if not already present.
		if t.cfg.injectXCloud && req.Header.Get("X-Cloud-Trace-Context") == "" {
			traceIDHex := sc.TraceID().String() // 32-char hex
			spanID := sc.SpanID()               // [8]byte
			spanIDUint := binary.BigEndian.Uint64(spanID[:])
			spanIDDec := strconv.FormatUint(spanIDUint, 10)

			sampled := "0"
			if sc.IsSampled() {
				sampled = "1"
			}
			req.Header.Set("X-Cloud-Trace-Context",
				fmt.Sprintf("%s/%s;o=%s", traceIDHex, spanIDDec, sampled))
		}
	}

	return t.base.RoundTrip(req)
}
