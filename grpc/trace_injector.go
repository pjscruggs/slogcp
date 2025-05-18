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
	"encoding/binary"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

const (
	// xCloudTraceContextKey is the lowercase metadata key for the GCP trace header.
	// gRPC metadata keys are automatically normalized to lowercase.
	xCloudTraceContextKey = "x-cloud-trace-context"
	// traceparentKey is the lowercase metadata key for the W3C trace context header.
	traceparentKey = "traceparent"
)

// wrappedServerStreamInjector wraps grpc.ServerStream to override the Context() method.
// This allows propagation of a modified context down the interceptor chain for
// streaming RPCs when using this manual trace injection interceptor.
type wrappedServerStreamInjector struct {
	grpc.ServerStream
	ctx context.Context
}

// Context returns the wrapped context, which may contain injected trace information.
func (w *wrappedServerStreamInjector) Context() context.Context {
	return w.ctx
}

// InjectUnaryTraceContextInterceptor returns a gRPC unary server interceptor
// that reads trace context headers (W3C traceparent or GCP X-Cloud-Trace-Context)
// from incoming request metadata and injects the parsed trace.SpanContext into
// the Go context.Context.
//
// This interceptor should only be used if standard OpenTelemetry instrumentation
// (like otelgrpc via stats.Handler with appropriate propagators) is NOT configured
// to handle trace propagation automatically. If standard OTel instrumentation is
// active, this interceptor will detect the existing trace context and do nothing,
// but it's recommended to omit it from the chain in that case to avoid minimal overhead.
//
// If used, place this interceptor early in the chain, after Recovery and Metrics,
// but before any interceptors that need to consume the trace context (like Logging).
func InjectUnaryTraceContextInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		// Check if standard OTel instrumentation has already populated the context.
		// If a valid span context exists, proceed without modifying the context.
		if trace.SpanContextFromContext(ctx).IsValid() {
			return handler(ctx, req)
		}

		// Attempt to extract trace context from headers and inject it.
		newCtx, ok := injectTraceFromMetadata(ctx)
		if !ok {
			// Headers not found or invalid, proceed with the original context.
			return handler(ctx, req)
		}

		// Proceed with the new context containing the injected trace info.
		return handler(newCtx, req)
	}
}

// InjectStreamTraceContextInterceptor returns a gRPC stream server interceptor
// that reads trace context headers (W3C traceparent or GCP X-Cloud-Trace-Context)
// from incoming request metadata and injects the parsed trace.SpanContext into
// the Go context.Context for the stream.
//
// This interceptor should only be used if standard OpenTelemetry instrumentation
// (like otelgrpc via stats.Handler with appropriate propagators) is NOT configured
// to handle trace propagation automatically. If standard OTel instrumentation is
// active, this interceptor will detect the existing trace context and do nothing,
// but it's recommended to omit it from the chain in that case to avoid minimal overhead.
//
// If used, place this interceptor early in the chain, after Recovery and Metrics,
// but before any interceptors that need to consume the trace context (like Logging).
func InjectStreamTraceContextInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx := ss.Context()

		// Check if standard OTel instrumentation has already populated the context.
		if trace.SpanContextFromContext(ctx).IsValid() {
			return handler(srv, ss)
		}

		// Attempt to extract trace context from headers and inject it.
		newCtx, ok := injectTraceFromMetadata(ctx)
		if !ok {
			// Headers not found or invalid, proceed with the original stream.
			return handler(srv, ss)
		}

		// Wrap the stream to carry the new context.
		wrappedStream := &wrappedServerStreamInjector{
			ServerStream: ss,
			ctx:          newCtx,
		}

		// Proceed with the wrapped stream.
		return handler(srv, wrappedStream)
	}
}

// injectTraceFromMetadata attempts to parse trace context from metadata and
// returns a new context with the injected SpanContext if successful.
// It prioritizes W3C traceparent over X-Cloud-Trace-Context.
func injectTraceFromMetadata(ctx context.Context) (context.Context, bool) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ctx, false // No metadata found.
	}

	// Prioritize W3C traceparent header.
	spanCtx, ok := parseTraceparent(md)
	if ok && spanCtx.IsValid() {
		// Use trace.ContextWithRemoteSpanContext to indicate the parent is remote.
		return trace.ContextWithRemoteSpanContext(ctx, spanCtx), true
	}

	// Fallback to X-Cloud-Trace-Context.
	spanCtx, ok = parseXCloudTraceContext(md)
	if ok && spanCtx.IsValid() {
		// Use trace.ContextWithRemoteSpanContext here as well.
		return trace.ContextWithRemoteSpanContext(ctx, spanCtx), true
	}

	// No valid trace context found in headers.
	return ctx, false
}

// parseTraceparent parses the W3C traceparent header from metadata.
// Returns a valid SpanContext and true if parsing is successful,
// otherwise returns an empty SpanContext and false.
func parseTraceparent(md metadata.MD) (trace.SpanContext, bool) {
	values := md.Get(traceparentKey) // Keys are lowercase in metadata.MD
	if len(values) == 0 {
		return trace.SpanContext{}, false
	}
	headerValue := values[0] // Use only the first value per W3C spec.

	// Format: version-traceid-spanid-flags
	parts := strings.Split(headerValue, "-")
	if len(parts) != 4 {
		return trace.SpanContext{}, false // Invalid format.
	}

	version, traceIDStr, spanIDStr, flagsStr := parts[0], parts[1], parts[2], parts[3]
	if version != "00" {
		return trace.SpanContext{}, false // Unsupported version.
	}

	traceID, err := trace.TraceIDFromHex(traceIDStr)
	if err != nil || !traceID.IsValid() {
		return trace.SpanContext{}, false // Invalid TraceID hex or zero value.
	}

	spanID, err := trace.SpanIDFromHex(spanIDStr)
	if err != nil || !spanID.IsValid() {
		return trace.SpanContext{}, false // Invalid SpanID hex or zero value.
	}

	flagsUint, err := strconv.ParseUint(flagsStr, 16, 8)
	if err != nil {
		return trace.SpanContext{}, false // Invalid flags hex.
	}
	// Corrected: Cast flagsUint to trace.TraceFlags before bitwise AND.
	// Check if the sampled bit (trace.FlagsSampled) is set in the parsed flags.
	sampled := (trace.TraceFlags(flagsUint) & trace.FlagsSampled) == trace.FlagsSampled
	traceFlags := trace.FlagsSampled.WithSampled(sampled)

	spanContextConfig := trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: traceFlags,
		Remote:     true, // Indicate this context originated remotely.
	}

	return trace.NewSpanContext(spanContextConfig), true
}

// parseXCloudTraceContext parses the GCP X-Cloud-Trace-Context header from metadata.
// Returns a valid SpanContext and true if parsing is successful,
// otherwise returns an empty SpanContext and false.
func parseXCloudTraceContext(md metadata.MD) (trace.SpanContext, bool) {
	values := md.Get(xCloudTraceContextKey) // Use lowercase key.
	if len(values) == 0 {
		return trace.SpanContext{}, false
	}
	headerValue := values[0]

	// Format: TRACE_ID/SPAN_ID;o=OPTIONS
	parts := strings.Split(headerValue, "/")
	if len(parts) != 2 {
		return trace.SpanContext{}, false // Invalid format.
	}
	traceIDStr := parts[0]

	spanIDStr := parts[1]
	optionsStr := ""
	if idx := strings.Index(spanIDStr, ";"); idx != -1 {
		optionsStr = spanIDStr[idx+1:]
		spanIDStr = spanIDStr[:idx]
	}

	traceID, err := trace.TraceIDFromHex(traceIDStr)
	if err != nil || !traceID.IsValid() {
		return trace.SpanContext{}, false // Invalid TraceID hex or zero value.
	}

	// SPAN_ID in XCTC is decimal uint64. Convert to hex SpanID.
	spanIDUint, err := strconv.ParseUint(spanIDStr, 10, 64)
	if err != nil {
		return trace.SpanContext{}, false // Invalid SpanID decimal.
	}
	var spanID trace.SpanID
	binary.BigEndian.PutUint64(spanID[:], spanIDUint) // Convert uint64 to 8-byte SpanID.
	if !spanID.IsValid() {
		return trace.SpanContext{}, false // Invalid SpanID (e.g., was zero).
	}

	// Parse sampling option (o=1 means sampled).
	var traceFlags trace.TraceFlags
	if strings.Contains(optionsStr, "o=1") {
		traceFlags = trace.FlagsSampled
	}

	spanContextConfig := trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: traceFlags,
		Remote:     true, // Indicate this context originated remotely.
	}

	return trace.NewSpanContext(spanContextConfig), true
}
