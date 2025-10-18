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
	"log/slog"
	"time"

	"github.com/pjscruggs/slogcp"
	"github.com/pjscruggs/slogcp/healthcheck"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

const streamServerInstrumentation = "github.com/pjscruggs/slogcp/grpc/serverstream"

// wrappedServerStream wraps an existing grpc.ServerStream to intercept SendMsg
// and RecvMsg calls. This allows for optional logging of message payloads
// without modifying the core interceptor logic significantly. It also ensures
// the correct context, potentially modified by preceding interceptors, is
// consistently returned by the Context() method.
type wrappedServerStream struct {
	grpc.ServerStream
	ctx    context.Context // The context associated with this stream, potentially modified by interceptors.
	logger *slog.Logger    // The logger instance used for payload logging.
	opts   *options        // Configuration options determining logging behavior (e.g., payload logging enabled).
}

// contextOnlyServerStream wraps an existing grpc.ServerStream but overrides only
// the Context() method. It is used when a stream is filtered out by logging
// rules (e.g., WithShouldLog, WithSkipPaths, sampling) so the handler still
// receives the enriched context from earlier interceptors (tracing/auth/etc.).
type contextOnlyServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

// Context returns the wrapped context so downstream handlers see any values
// injected by earlier interceptors (e.g., tracing/auth). It performs no other
// behavior change and introduces no logging side effects.
func (s *contextOnlyServerStream) Context() context.Context { return s.ctx }

// RecvMsg calls the underlying ServerStream's RecvMsg method. If the call is
// successful and payload logging is enabled in the options, it logs the
// received message payload using the stored context and logger.
func (w *wrappedServerStream) RecvMsg(m interface{}) error {
	err := w.ServerStream.RecvMsg(m)
	if err != nil {
		// Do not log payload on receive error (e.g., EOF, connection error).
		// The main interceptor logs the final stream status based on this error.
		return err
	}
	// Log payload only if enabled and the receive was successful.
	if !w.opts.logPayloads {
		return nil
	}
	// Log using the context stored in the wrapper to ensure trace correlation.
	logPayload(w.ctx, w.logger, w.opts, "received", m)
	return nil
}

// SendMsg logs the outgoing message payload if payload logging is enabled,
// then calls the underlying ServerStream's SendMsg method.
func (w *wrappedServerStream) SendMsg(m interface{}) error {
	// Log payload before attempting to send, as SendMsg might fail.
	if w.opts.logPayloads {
		// Log using the context stored in the wrapper.
		logPayload(w.ctx, w.logger, w.opts, "sent", m)
	}
	// Call the underlying stream's SendMsg.
	err := w.ServerStream.SendMsg(m)
	// Do not log send errors here; the main interceptor logs the final stream status.
	return err
}

// Context returns the context associated with this wrapped stream. This ensures
// that any modifications made to the context by interceptors before this wrapper
// are preserved and accessible to the application handler and subsequent logging.
func (w *wrappedServerStream) Context() context.Context {
	return w.ctx
}

// StreamServerInterceptor returns a new streaming server interceptor ([grpc.StreamServerInterceptor])
// that logs incoming gRPC streams using the provided [slog.Logger].
//
// It automatically logs the gRPC service and method name, stream duration, final
// gRPC status code, peer address, and any error returned by the handler upon
// completion of the stream. Trace context is extracted for correlation. It also
// recovers from panics in the handler, logs the panic with a stack trace, and
// returns a codes.Internal error.
//
// The slogcp.Handler configured with the logger is responsible for automatically
// extracting trace context (TraceID, SpanID, Sampled) from the context.Context
// passed to logging methods and including the appropriate `logging.googleapis.com/...`
// fields in the final log entry. This interceptor ensures the context is passed correctly.
//
// If configured via [WithMetadataLogging](true), it logs filtered incoming request
// metadata. Note that outgoing headers/trailers set via [grpc.SetHeader] or
// [grpc.SetTrailer] are not captured by this interceptor.
//
// If configured via [WithPayloadLogging](true), it also logs individual messages
// sent and received on the stream at [slog.LevelDebug]. Payload size can be limited
// using [WithMaxPayloadSize].
//
// Behavior can be further customized using other [Option] functions like [WithLevels]
// and [WithShouldLog]. By default, all streams are logged, payloads are not logged,
// and a standard mapping from gRPC status codes to log levels is used.
func StreamServerInterceptor(logger *slog.Logger, opts ...Option) grpc.StreamServerInterceptor {
	// Process functional options to get the final configuration.
	cfg := processOptions(opts...)

	// Return the actual interceptor function.
	return func(
		srv interface{}, // The service implementation.
		ss grpc.ServerStream, // The incoming server stream.
		info *grpc.StreamServerInfo, // Information about the stream.
		handler grpc.StreamHandler, // The next interceptor or the final stream handler.
	) (err error) { // Named return value allows modification in defer.

		startTime := time.Now()
		// Get context early for filtering and start log.
		// This context should contain trace info if populated by preceding interceptors (e.g., OTel).
		ctx := ss.Context()

		// Extract inbound trace context from metadata via global OTel propagator,
		// fallback to X-Cloud-Trace-Context if none found.
		var incomingMD metadata.MD
		if md, ok := metadata.FromIncomingContext(ctx); ok {
			incomingMD = md
			ctxExtracted := otel.GetTextMapPropagator().Extract(ctx, metadataCarrier{md: md})
			if !trace.SpanContextFromContext(ctxExtracted).IsValid() {
				if vals := md.Get(xCloudTraceContextHeaderMD); len(vals) > 0 {
					ctxExtracted = injectTraceContextFromXCloudHeader(ctxExtracted, vals[0])
				}
			}
			ctx = ctxExtracted
		}

		tracer := cfg.tracer
		if tracer == nil {
			tracer = otel.Tracer(streamServerInstrumentation)
		}

		var startedSpan trace.Span
		if cfg.startSpanIfAbsent && !trace.SpanContextFromContext(ctx).IsValid() {
			ctx, startedSpan = tracer.Start(ctx, info.FullMethod, trace.WithSpanKind(trace.SpanKindServer))
			defer startedSpan.End()
		}

		peerAddr := "unknown"
		if p, ok := peer.FromContext(ctx); ok {
			peerAddr = p.Addr.String()
		}

		var hcDecision healthcheck.Decision
		if cfg.healthCheckFilter != nil {
			var metadataMap map[string][]string
			if len(incomingMD) > 0 {
				metadataMap = metadataToHeader(incomingMD)
			}
			hcDecision = cfg.healthCheckFilter.MatchGRPC(info.FullMethod, metadataMap, peerAddr)
			if hcDecision.Matched {
				ctx = healthcheck.ContextWithDecision(ctx, hcDecision)
			}
		}

		activeLogger := logger
		if cfg.attachLogger {
			reqLogger := logger
			if attrs, ok := slogcp.TraceAttributes(ctx, cfg.traceProjectID); ok {
				reqLogger = loggerWithAttrs(reqLogger, attrs)
			}
			if hcDecision.ShouldTag() {
				reqLogger = reqLogger.With(slog.Bool(hcDecision.TagKey, hcDecision.TagValue))
			}
			activeLogger = reqLogger
			ctx = slogcp.ContextWithLogger(ctx, reqLogger)
		}

		// Check if this call should be logged based on the filter function.
		shouldLog := cfg.shouldLogFunc(ctx, info.FullMethod)
		if hcDecision.Matched && hcDecision.Mode == healthcheck.ModeDrop {
			shouldLog = false
		}
		if !shouldLog {
			// Ensure the handler sees the enriched context, but do not attach
			// any logging hooks.
			streamToUse := &contextOnlyServerStream{
				ServerStream: ss,
				ctx:          ctx,
			}
			return handler(srv, streamToUse)
		}

		serviceName, methodName := splitMethodName(info.FullMethod)

		// Prepare metadata attribute slice early if needed for the final log.
		var metadataAttrs []slog.Attr
		if cfg.logMetadata {
			// Extract and filter incoming metadata *before* calling the handler.
			filteredIncomingMD := filterMetadata(incomingMD, cfg.metadataFilterFunc)
			if filteredIncomingMD != nil {
				metadataAttrs = append(metadataAttrs, slog.Group(grpcRequestMetadataKey, slog.Any(metadataValuesKey, filteredIncomingMD)))
			}
		}

		// Log the "start" event.
		// Pass the original context (ctx) so the handler can extract trace info.
		startLevel := hcDecision.ApplyLevel(slog.LevelInfo)
		startAttrs := []slog.Attr{
			slog.String(grpcServiceKey, serviceName),
			slog.String(grpcMethodKey, methodName),
			slog.String(peerAddressKey, peerAddr),
		}
		if cfg.logCategory != "" {
			startAttrs = append(startAttrs, slog.String(categoryKey, cfg.logCategory))
		}
		if hcDecision.ShouldTag() {
			startAttrs = append(startAttrs, slog.Bool(hcDecision.TagKey, hcDecision.TagValue))
		}
		activeLogger.LogAttrs(ctx, startLevel, "Starting gRPC stream", startAttrs...)

		// Setup panic recovery and final logging. This defer runs after the handler returns or panics.
		defer func() {
			// Handle potential panics from the handler.
			// Pass logger and context to handlePanic for immediate logging.
			// handlePanic uses the logger directly, which uses the context-aware handler.
			isPanic, panicErr := handlePanic(ctx, activeLogger, recover()) // Pass embedded *slog.Logger
			if isPanic {
				err = panicErr // Overwrite handler error with internal error on panic.
			}

			// Calculate duration and determine final status/level.
			duration := time.Since(startTime)
			level := cfg.levelFunc(status.Code(err)) // Determine level based on final error.
			if isPanic {
				level = internalLevelCritical // Override level for panics.
			}
			level = hcDecision.ApplyLevel(level)

			// Assemble final log attributes using helpers.
			finishAttrs := assembleFinishAttrs(duration, err, peerAddr)
			// Trace attributes are handled automatically by the slogcp.Handler via context.
			// Panic attributes are logged directly in handlePanic.

			// Combine all attribute slices.
			// Pre-allocate slice: 4 base attrs + metadata attrs. Trace attrs added by handler.
			logAttrs := make([]slog.Attr, 0, 4+len(metadataAttrs))
			logAttrs = append(logAttrs, slog.String(grpcServiceKey, serviceName))
			logAttrs = append(logAttrs, slog.String(grpcMethodKey, methodName))
			if cfg.logCategory != "" {
				logAttrs = append(logAttrs, slog.String(categoryKey, cfg.logCategory))
			}
			logAttrs = append(logAttrs, finishAttrs...)
			logAttrs = append(logAttrs, metadataAttrs...)
			if hcDecision.ShouldTag() {
				logAttrs = append(logAttrs, slog.Bool(hcDecision.TagKey, hcDecision.TagValue))
			}

			logMsg := "Finished gRPC stream"
			// Panic message is logged separately in handlePanic.
			// We still log the finish event, but the error reflects the panic.
			if isPanic {
				logMsg = "Finished gRPC stream after panic recovery"
			}
			// Log the final completion event using the original context (ctx).
			// The handler will extract trace info from ctx.
			activeLogger.LogAttrs(ctx, level, logMsg, logAttrs...)

		}() // End of defer function for panic recovery and logging.

		// Always wrap the stream so the handler sees the enriched context via Context().
		streamToUse := &wrappedServerStream{
			ServerStream: ss,
			ctx:          ctx,
			logger:       activeLogger,
			opts:         cfg,
		}

		// Call the actual gRPC stream handler with the wrapped stream.
		err = handler(srv, streamToUse)

		// Return the error (either from the handler or from panic recovery).
		return err
	}
}
