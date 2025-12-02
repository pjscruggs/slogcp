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
	"crypto/rand"
	"encoding/binary"
	"strconv"
	"strings"

	stdhttp "net/http"

	"go.opentelemetry.io/otel/trace"
)

// XCloudTraceContextHeader is the Google Cloud legacy trace propagation header.
const XCloudTraceContextHeader = "X-Cloud-Trace-Context"

var randRead = rand.Read

// InjectTraceContextMiddleware extracts legacy X-Cloud-Trace-Context headers
// when no OpenTelemetry span is present in the incoming context. The extracted
// span context becomes the active context for downstream handlers.
func InjectTraceContextMiddleware() func(stdhttp.Handler) stdhttp.Handler {
	return func(next stdhttp.Handler) stdhttp.Handler {
		return stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
			ctx := r.Context()
			if trace.SpanContextFromContext(ctx).IsValid() {
				next.ServeHTTP(w, r)
				return
			}
			header := r.Header.Get(XCloudTraceContextHeader)
			if header == "" {
				next.ServeHTTP(w, r)
				return
			}
			if sc, ok := parseXCloudTrace(header); ok {
				ctx = trace.ContextWithRemoteSpanContext(ctx, sc)
				r = r.WithContext(ctx)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// contextWithXCloudTrace augments ctx with a span context parsed from the legacy header.
func contextWithXCloudTrace(ctx context.Context, header string) (context.Context, bool) {
	sc, ok := parseXCloudTrace(header)
	if !ok {
		return ctx, false
	}
	return trace.ContextWithRemoteSpanContext(ctx, sc), true
}

// parseXCloudTrace decodes an X-Cloud-Trace-Context header into a span context.
func parseXCloudTrace(header string) (trace.SpanContext, bool) {
	parts, ok := splitXCloudTraceHeader(header)
	if !ok {
		return trace.SpanContext{}, false
	}

	traceID, err := trace.TraceIDFromHex(parts.traceID)
	if err != nil || !traceID.IsValid() {
		return trace.SpanContext{}, false
	}

	spanID, ok := parseSpanID(parts.spanDecimal)
	if !ok {
		return trace.SpanContext{}, false
	}
	flags := traceFlagsFromOptions(parts.options)

	return buildSpanContext(traceID, spanID, flags)
}

type xCloudTraceParts struct {
	traceID     string
	spanDecimal string
	options     string
}

// splitXCloudTraceHeader separates the trace ID, span ID, and options fields.
func splitXCloudTraceHeader(header string) (xCloudTraceParts, bool) {
	header = strings.TrimSpace(header)
	if header == "" {
		return xCloudTraceParts{}, false
	}

	idPart, options, _ := strings.Cut(header, ";")
	idPart = strings.TrimSpace(idPart)
	if idPart == "" {
		return xCloudTraceParts{}, false
	}

	traceIDStr := idPart
	spanDecimal := ""
	if parts := strings.SplitN(idPart, "/", 2); len(parts) == 2 {
		traceIDStr = strings.TrimSpace(parts[0])
		spanDecimal = strings.TrimSpace(parts[1])
	}

	return xCloudTraceParts{
		traceID:     traceIDStr,
		spanDecimal: spanDecimal,
		options:     options,
	}, true
}

// parseSpanID converts decimal span IDs into the binary form expected by trace.SpanID.
func parseSpanID(spanDecimal string) (trace.SpanID, bool) {
	var spanID trace.SpanID
	if spanDecimal != "" {
		if spanUint, err := strconv.ParseUint(spanDecimal, 10, 64); err == nil {
			binary.BigEndian.PutUint64(spanID[:], spanUint)
		}
	}
	if spanID.IsValid() {
		return spanID, true
	}
	if _, err := randRead(spanID[:]); err != nil {
		return trace.SpanID{}, false
	}
	return spanID, true
}

// traceFlagsFromOptions extracts sampling flags from the options suffix.
func traceFlagsFromOptions(options string) trace.TraceFlags {
	if strings.Contains(options, "o=1") {
		return trace.FlagsSampled
	}
	return 0
}

// buildSpanContext validates and constructs a remote span context.
func buildSpanContext(traceID trace.TraceID, spanID trace.SpanID, flags trace.TraceFlags) (trace.SpanContext, bool) {
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: flags,
		Remote:     true,
	})
	if !sc.IsValid() {
		return trace.SpanContext{}, false
	}
	return sc, true
}
