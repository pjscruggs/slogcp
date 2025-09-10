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
	"crypto/rand"
	"encoding/binary"
	"fmt"
	stdhttp "net/http"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

const (
	// XCloudTraceContextHeader is the header name used by GCP for trace propagation.
	XCloudTraceContextHeader = "X-Cloud-Trace-Context"
)

// InjectTraceContextMiddleware creates middleware that extracts trace context from
// the X-Cloud-Trace-Context header and injects it into the request context.
//
// This middleware should only be used if standard OpenTelemetry instrumentation
// isn't configured to handle this header format. If OTel instrumentation is already
// active, this middleware detects existing trace context and takes no action.
//
// Add this middleware early in the chain, before any middleware that needs
// access to trace context.
func InjectTraceContextMiddleware() func(stdhttp.Handler) stdhttp.Handler {
	return func(next stdhttp.Handler) stdhttp.Handler {
		return stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
			// Skip if trace context already exists from other instrumentation.
			if trace.SpanContextFromContext(r.Context()).IsValid() {
				next.ServeHTTP(w, r)
				return
			}

			// Extract from X-Cloud-Trace-Context header.
			header := r.Header.Get(XCloudTraceContextHeader)
			if header == "" {
				next.ServeHTTP(w, r)
				return
			}

			ctx := injectTraceContextFromHeader(r.Context(), header)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// injectTraceContextFromHeader parses the X-Cloud-Trace-Context header value and
// creates a trace context that can be used by OpenTelemetry-aware systems.
//
// The header format is "TRACE_ID[/SPAN_ID][;o=OPTION]" where:
//   - TRACE_ID is a 32-character hex string
//   - SPAN_ID is a decimal number (converted to hex for internal use)
//   - OPTION is "1" if the request is sampled, "0" or omitted otherwise
func injectTraceContextFromHeader(ctx context.Context, header string) context.Context {
	// Split once on ';' to separate options (if present).
	idPart := header
	optionsStr := ""
	if semi := strings.Index(header, ";"); semi != -1 {
		idPart = header[:semi]
		optionsStr = header[semi+1:]
	}

	// idPart may be just TRACE_ID or TRACE_ID/SPAN_ID
	idParts := strings.SplitN(idPart, "/", 2)
	traceIDStr := idParts[0]
	spanIDPart := ""
	if len(idParts) == 2 {
		spanIDPart = idParts[1]
	}

	traceID, err := trace.TraceIDFromHex(traceIDStr)
	if err != nil || !traceID.IsValid() {
		return ctx // Invalid trace ID
	}

	// Parse sampling option (o=1 means sampled).
	var traceFlags trace.TraceFlags
	if strings.Contains(optionsStr, "o=1") {
		traceFlags = trace.FlagsSampled
	}

	var spanID trace.SpanID
	if spanIDPart == "" {
		// No span provided: generate a new one.
		if _, genErr := rand.Read(spanID[:]); genErr != nil {
			return ctx
		}
	} else {
		// In X-Cloud-Trace-Context, span ID is decimal uint64.
		spanIDUint, err := strconv.ParseUint(spanIDPart, 10, 64)
		if err != nil {
			// Invalid span ID, generate a new one to use with the trace.
			if _, genErr := rand.Read(spanID[:]); genErr != nil {
				return ctx
			}
		} else {
			// Convert decimal span ID to trace.SpanID bytes (big-endian).
			binary.BigEndian.PutUint64(spanID[:], spanIDUint)
		}
	}

	if !spanID.IsValid() {
		return ctx // Invalid span ID
	}

	return trace.ContextWithRemoteSpanContext(ctx, trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: traceFlags,
		Remote:     true,
	}))
}

// TracePropagationTransport is an http.RoundTripper that propagates the current
// trace context on outbound HTTP requests. It injects a W3C traceparent header
// (via the global OpenTelemetry propagator) and, by default, also injects the
// Google legacy X-Cloud-Trace-Context header for compatibility.
//
// The transport is safe to stack with other transports. It does not overwrite
// headers already present on the request.
type TracePropagationTransport struct {
	// Base is the underlying transport used to execute requests. If nil,
	// http.DefaultTransport is used.
	Base stdhttp.RoundTripper

	// InjectW3C controls injection of the W3C trace headers (traceparent/tracestate).
	// When both InjectW3C and InjectXCloud are false, both are treated as true.
	InjectW3C bool

	// InjectXCloud controls injection of the Google X-Cloud-Trace-Context header.
	// When both InjectW3C and InjectXCloud are false, both are treated as true.
	InjectXCloud bool

	// Skip, if set, suppresses propagation for matching requests.
	// When Skip returns true, the request is forwarded without adding headers.
	Skip func(*stdhttp.Request) bool
}

// RoundTrip implements http.RoundTripper. It injects trace context headers
// into req based on the active span in req.Context(), then delegates to Base.
func (t TracePropagationTransport) RoundTrip(req *stdhttp.Request) (*stdhttp.Response, error) {
	base := t.Base
	if base == nil {
		base = stdhttp.DefaultTransport
	}
	if t.Skip != nil && t.Skip(req) {
		return base.RoundTrip(req)
	}

	injectW3C := t.InjectW3C
	injectX := t.InjectXCloud
	if !t.InjectW3C && !t.InjectXCloud {
		// Zero-value convenience: enable both unless explicitly disabled.
		injectW3C, injectX = true, true
	}

	ctx := req.Context()
	sc := trace.SpanContextFromContext(ctx)
	if sc.IsValid() {
		// Do not overwrite if headers already present (e.g., otelhttp or a mesh).
		if injectW3C && req.Header.Get("traceparent") == "" {
			otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
		}
		if injectX && req.Header.Get(XCloudTraceContextHeader) == "" {
			// X-Cloud-Trace-Context format:
			//   TRACE_ID_HEX / SPAN_ID_DECIMAL ; o=1|0
			sampled := 0
			if sc.IsSampled() {
				sampled = 1
			}
			// Convert SpanID (8 bytes) to uint64 decimal as required by X-Cloud.
			sid := sc.SpanID() // local variable makes the array addressable
			decSpan := binary.BigEndian.Uint64(sid[:])
			req.Header.Set(
				XCloudTraceContextHeader,
				fmt.Sprintf("%s/%d;o=%d", sc.TraceID().String(), decSpan, sampled),
			)
		}
	}

	return base.RoundTrip(req)
}
