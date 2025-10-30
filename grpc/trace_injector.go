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

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
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

// injectTraceFromMetadata delegates trace context extraction to the globally
// configured OpenTelemetry text map propagator. It returns a new context
// carrying the extracted remote span when successful.
func injectTraceFromMetadata(ctx context.Context) (context.Context, bool) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok || len(md) == 0 {
		return ctx, false
	}

	newCtx := otel.GetTextMapPropagator().Extract(ctx, metadataCarrier{md: md})
	if sc := trace.SpanContextFromContext(newCtx); sc.IsValid() {
		return newCtx, true
	}

	return ctx, false
}
