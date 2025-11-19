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

package grpc

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"strconv"
	"strings"

	"github.com/pjscruggs/slogcp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/metadata"
)

const XCloudTraceContextHeader = "X-Cloud-Trace-Context"

var randReader = rand.Read

type metadataCarrier struct {
	metadata.MD
}

// Get returns the first value for the provided metadata key.
func (mc metadataCarrier) Get(key string) string {
	values := mc.MD.Get(key)
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

// Set stores the value under the provided metadata key.
func (mc metadataCarrier) Set(key string, value string) {
	mc.MD.Set(key, value)
}

// Keys reports all metadata keys present in the carrier.
func (mc metadataCarrier) Keys() []string {
	keys := make([]string, 0, len(mc.MD))
	for k := range mc.MD {
		keys = append(keys, k)
	}
	return keys
}

// ensureServerSpanContext obtains a remote span context from request metadata or propagation.
func ensureServerSpanContext(ctx context.Context, md metadata.MD, cfg *config) (context.Context, trace.SpanContext) {
	sc := trace.SpanContextFromContext(ctx)
	if sc.IsValid() {
		return ctx, sc
	}

	propagator := cfg.propagators
	if propagator == nil {
		propagator = otel.GetTextMapPropagator()
	}
	if propagator != nil {
		extracted := propagator.Extract(ctx, metadataCarrier{md})
		sc = trace.SpanContextFromContext(extracted)
		if sc.IsValid() {
			return extracted, sc
		}
	}

	if val := first(md, "grpc-trace-bin"); val != "" {
		if sc, ok := parseGRPCTraceBin(val); ok {
			newCtx := trace.ContextWithRemoteSpanContext(ctx, sc)
			return newCtx, sc
		}
	}

	if val := first(md, "traceparent"); val != "" {
		tc := propagation.TraceContext{}
		extracted := tc.Extract(ctx, metadataCarrier{md})
		sc = trace.SpanContextFromContext(extracted)
		if sc.IsValid() {
			return extracted, sc
		}
	}

	if val := first(md, strings.ToLower(XCloudTraceContextHeader)); val != "" {
		if newCtx, ok := contextWithXCloudTrace(ctx, val); ok {
			return newCtx, trace.SpanContextFromContext(newCtx)
		}
	}

	return ctx, sc
}

// injectClientTrace injects tracing metadata for outbound RPCs, including optional legacy headers.
func injectClientTrace(ctx context.Context, md metadata.MD, cfg *config) {
	propagator := cfg.propagators
	if propagator == nil {
		propagator = otel.GetTextMapPropagator()
	}
	if propagator != nil {
		propagator.Inject(ctx, metadataCarrier{md})
	}

	if !cfg.injectLegacyXCTC {
		return
	}

	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return
	}
	if existing := first(md, strings.ToLower(XCloudTraceContextHeader)); existing != "" {
		return
	}

	value := slogcp.BuildXCloudTraceContext(sc.TraceID().String(), sc.SpanID().String(), sc.IsSampled())
	md.Set(XCloudTraceContextHeader, value)
}

// parseGRPCTraceBin decodes the grpc-trace-bin header into a span context.
func parseGRPCTraceBin(val string) (trace.SpanContext, bool) {
	if val == "" {
		return trace.SpanContext{}, false
	}
	data, err := base64.StdEncoding.DecodeString(val)
	if err != nil {
		return trace.SpanContext{}, false
	}
	if len(data) < 29 || data[0] != 0 {
		return trace.SpanContext{}, false
	}
	offset := 1
	if data[offset] != 0 || len(data) < offset+1+16 {
		return trace.SpanContext{}, false
	}
	offset++
	var traceID trace.TraceID
	copy(traceID[:], data[offset:offset+16])
	offset += 16
	if data[offset] != 1 || len(data) < offset+1+8 {
		return trace.SpanContext{}, false
	}
	offset++
	var spanID trace.SpanID
	copy(spanID[:], data[offset:offset+8])
	offset += 8
	if data[offset] != 2 || len(data) < offset+2 {
		return trace.SpanContext{}, false
	}
	offset++
	flags := trace.TraceFlags(data[offset])

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

// first returns the first metadata value for the provided key.
func first(md metadata.MD, key string) string {
	if md == nil {
		return ""
	}
	values := md.Get(key)
	if len(values) > 0 {
		return values[0]
	}
	return ""
}

// contextWithXCloudTrace augments the context with a span context parsed from X-Cloud-Trace-Context.
func contextWithXCloudTrace(ctx context.Context, header string) (context.Context, bool) {
	sc, ok := parseXCloudTrace(header)
	if !ok {
		return ctx, false
	}
	return trace.ContextWithRemoteSpanContext(ctx, sc), true
}

// parseXCloudTrace parses the legacy X-Cloud-Trace-Context header into a span context.
func parseXCloudTrace(header string) (trace.SpanContext, bool) {
	if header == "" {
		return trace.SpanContext{}, false
	}

	idPart := header
	options := ""
	if cut, opts, ok := strings.Cut(header, ";"); ok {
		idPart = cut
		options = opts
	}

	idPart = strings.TrimSpace(idPart)
	if idPart == "" {
		return trace.SpanContext{}, false
	}

	traceIDStr := idPart
	spanDecimal := ""
	if parts := strings.SplitN(idPart, "/", 2); len(parts) == 2 {
		traceIDStr = strings.TrimSpace(parts[0])
		spanDecimal = strings.TrimSpace(parts[1])
	}

	traceID, err := trace.TraceIDFromHex(traceIDStr)
	if err != nil || !traceID.IsValid() {
		return trace.SpanContext{}, false
	}

	var spanID trace.SpanID
	if spanDecimal != "" {
		if spanUint, err := strconv.ParseUint(spanDecimal, 10, 64); err == nil {
			binary.BigEndian.PutUint64(spanID[:], spanUint)
		}
	}
	if !spanID.IsValid() {
		if _, err := randReader(spanID[:]); err != nil {
			return trace.SpanContext{}, false
		}
	}

	var flags trace.TraceFlags
	if strings.Contains(options, "o=1") {
		flags = trace.FlagsSampled
	}

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
