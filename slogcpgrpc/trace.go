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

package slogcpgrpc

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/metadata"

	"github.com/pjscruggs/slogcp"
)

// XCloudTraceContextHeader is the canonical header name for Cloud Trace context propagation.
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
	if cfg != nil && !cfg.propagateTrace {
		return ctx, trace.SpanContextFromContext(ctx)
	}

	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		return ctx, sc
	}

	if newCtx, sc := extractWithPropagator(ctx, md, cfg); sc.IsValid() {
		return newCtx, sc
	}
	if newCtx, sc := extractFromHeaders(ctx, md); sc.IsValid() {
		return newCtx, sc
	}

	return ctx, trace.SpanContextFromContext(ctx)
}

// extractWithPropagator uses configured propagators to pull trace context from metadata.
func extractWithPropagator(ctx context.Context, md metadata.MD, cfg *config) (context.Context, trace.SpanContext) {
	propagator := cfg.propagators
	if propagator == nil {
		propagator = otel.GetTextMapPropagator()
	}
	if propagator == nil {
		return ctx, trace.SpanContextFromContext(ctx)
	}
	extracted := propagator.Extract(ctx, metadataCarrier{md})
	return extracted, trace.SpanContextFromContext(extracted)
}

// extractFromHeaders attempts to parse known trace headers in priority order.
func extractFromHeaders(ctx context.Context, md metadata.MD) (context.Context, trace.SpanContext) {
	if newCtx, sc := extractGRPCTraceBin(ctx, md); sc.IsValid() {
		return newCtx, sc
	}
	if newCtx, sc := extractTraceParent(ctx, md); sc.IsValid() {
		return newCtx, sc
	}
	if newCtx, sc := extractXCloudTraceHeader(ctx, md); sc.IsValid() {
		return newCtx, sc
	}
	return ctx, trace.SpanContextFromContext(ctx)
}

// extractGRPCTraceBin parses grpc-trace-bin headers into a span context.
func extractGRPCTraceBin(ctx context.Context, md metadata.MD) (context.Context, trace.SpanContext) {
	if val := first(md, "grpc-trace-bin"); val != "" {
		if sc, ok := parseGRPCTraceBin(val); ok {
			return trace.ContextWithRemoteSpanContext(ctx, sc), sc
		}
	}
	return ctx, trace.SpanContextFromContext(ctx)
}

// extractTraceParent parses W3C traceparent headers from metadata.
func extractTraceParent(ctx context.Context, md metadata.MD) (context.Context, trace.SpanContext) {
	if val := first(md, "traceparent"); val == "" {
		return ctx, trace.SpanContextFromContext(ctx)
	}
	tc := propagation.TraceContext{}
	extracted := tc.Extract(ctx, metadataCarrier{md})
	return extracted, trace.SpanContextFromContext(extracted)
}

// extractXCloudTraceHeader decodes legacy Cloud Trace headers from metadata.
func extractXCloudTraceHeader(ctx context.Context, md metadata.MD) (context.Context, trace.SpanContext) {
	val := first(md, strings.ToLower(XCloudTraceContextHeader))
	if val == "" {
		return ctx, trace.SpanContextFromContext(ctx)
	}
	if newCtx, ok := contextWithXCloudTrace(ctx, val); ok {
		return newCtx, trace.SpanContextFromContext(newCtx)
	}
	return ctx, trace.SpanContextFromContext(ctx)
}

// injectClientTrace injects tracing metadata for outbound RPCs, including optional legacy headers.
func injectClientTrace(ctx context.Context, md metadata.MD, cfg *config) {
	if cfg != nil && !cfg.propagateTrace {
		return
	}

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
	parser := newGRPCTraceBinParser(val)
	if parser == nil {
		return trace.SpanContext{}, false
	}
	traceID, ok := parser.traceID()
	if !ok {
		return trace.SpanContext{}, false
	}
	spanID, ok := parser.spanID()
	if !ok {
		return trace.SpanContext{}, false
	}
	flags, ok := parser.flags()
	if !ok {
		return trace.SpanContext{}, false
	}

	return validateSpanContext(traceID, spanID, flags)
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
	parts, ok := splitXCloudTraceHeader(header)
	if !ok {
		return trace.SpanContext{}, false
	}
	traceID, err := trace.TraceIDFromHex(parts.traceID)
	if err != nil || !validateTraceID(traceID) {
		return trace.SpanContext{}, false
	}

	spanID, ok := parseSpanID(parts.spanDecimal)
	if !ok {
		return trace.SpanContext{}, false
	}
	flags := traceFlagsFromOptions(parts.options)

	return validateSpanContext(traceID, spanID, flags)
}

type grpcTraceBinParser struct {
	data   []byte
	offset int
}

// newGRPCTraceBinParser builds a parser for the grpc-trace-bin header.
func newGRPCTraceBinParser(val string) *grpcTraceBinParser {
	if val == "" {
		return nil
	}
	data, err := base64.StdEncoding.DecodeString(val)
	if err != nil || len(data) < 29 || data[0] != 0 {
		return nil
	}
	return &grpcTraceBinParser{data: data, offset: 1}
}

// consume validates the upcoming field marker and returns the corresponding slice.
func (p *grpcTraceBinParser) consume(expected byte, length int) ([]byte, bool) {
	if p.offset >= len(p.data) || p.data[p.offset] != expected || len(p.data) < p.offset+1+length {
		return nil, false
	}
	p.offset++
	next := p.data[p.offset : p.offset+length]
	p.offset += length
	return next, true
}

// traceID extracts a TraceID from the binary header.
func (p *grpcTraceBinParser) traceID() (trace.TraceID, bool) {
	var traceID trace.TraceID
	data, ok := p.consume(0, 16)
	if !ok {
		return trace.TraceID{}, false
	}
	copy(traceID[:], data)
	return traceID, true
}

// spanID extracts a SpanID from the binary header.
func (p *grpcTraceBinParser) spanID() (trace.SpanID, bool) {
	var spanID trace.SpanID
	data, ok := p.consume(1, 8)
	if !ok {
		return trace.SpanID{}, false
	}
	copy(spanID[:], data)
	return spanID, true
}

// flags extracts trace flags from the binary header.
func (p *grpcTraceBinParser) flags() (trace.TraceFlags, bool) {
	if p.offset >= len(p.data) || p.data[p.offset] != 2 || len(p.data) < p.offset+2 {
		return 0, false
	}
	p.offset++
	return trace.TraceFlags(p.data[p.offset]), true
}

type xCloudTraceParts struct {
	traceID     string
	spanDecimal string
	options     string
}

// splitXCloudTraceHeader separates trace ID, span, and options fields.
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

// validateTraceID reports whether a TraceID is usable.
func validateTraceID(traceID trace.TraceID) bool {
	return traceID.IsValid()
}

// parseSpanID converts decimal span IDs to TraceID format, generating a random ID when empty.
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
	if _, err := randReader(spanID[:]); err != nil {
		return trace.SpanID{}, false
	}
	return spanID, true
}

// traceFlagsFromOptions extracts sampling flags from header options.
func traceFlagsFromOptions(options string) trace.TraceFlags {
	if strings.Contains(options, "o=1") {
		return trace.FlagsSampled
	}
	return 0
}

// validateSpanContext builds and validates a remote span context.
func validateSpanContext(traceID trace.TraceID, spanID trace.SpanID, flags trace.TraceFlags) (trace.SpanContext, bool) {
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
