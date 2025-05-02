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
	"net/http"
	"strconv"
	"strings"

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
func InjectTraceContextMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Skip if trace context already exists from other instrumentation
			if trace.SpanContextFromContext(r.Context()).IsValid() {
				next.ServeHTTP(w, r)
				return
			}

			// Extract from X-Cloud-Trace-Context header
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
// The header format is "TRACE_ID/SPAN_ID;o=OPTION" where:
// - TRACE_ID is a 32-character hex string
// - SPAN_ID is a decimal number (converted to hex for internal use)
// - OPTION is "1" if the request is sampled, "0" or omitted otherwise
func injectTraceContextFromHeader(ctx context.Context, header string) context.Context {
	// Format: TRACE_ID/SPAN_ID;o=OPTIONS
	parts := strings.Split(header, "/")
	if len(parts) != 2 {
		return ctx // Invalid format
	}

	traceIDStr := parts[0]
	spanIDPart := parts[1]
	optionsStr := ""

	if idx := strings.Index(spanIDPart, ";"); idx != -1 {
		optionsStr = spanIDPart[idx+1:]
		spanIDPart = spanIDPart[:idx]
	}

	traceID, err := trace.TraceIDFromHex(traceIDStr)
	if err != nil || !traceID.IsValid() {
		return ctx // Invalid trace ID
	}

	// In X-Cloud-Trace-Context, span ID is decimal uint64
	spanIDUint, err := strconv.ParseUint(spanIDPart, 10, 64)
	if err != nil {
		// Invalid span ID, generate a new one to use with the trace
		var spanID trace.SpanID
		_, err = rand.Read(spanID[:])
		if err != nil {
			return ctx // Failed to generate span ID
		}

		// Parse sampling option (o=1 means sampled)
		sampled := strings.Contains(optionsStr, "o=1")
		traceFlags := trace.FlagsSampled.WithSampled(sampled)

		return trace.ContextWithRemoteSpanContext(ctx, trace.NewSpanContext(trace.SpanContextConfig{
			TraceID:    traceID,
			SpanID:     spanID,
			TraceFlags: traceFlags,
			Remote:     true,
		}))
	}

	// Convert decimal span ID to trace.SpanID bytes
	var spanID trace.SpanID
	binary.BigEndian.PutUint64(spanID[:], spanIDUint)
	if !spanID.IsValid() {
		return ctx // Invalid span ID
	}

	// Parse sampling option (o=1 means sampled)
	sampled := strings.Contains(optionsStr, "o=1")
	traceFlags := trace.FlagsSampled.WithSampled(sampled)

	return trace.ContextWithRemoteSpanContext(ctx, trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: traceFlags,
		Remote:     true,
	}))
}
