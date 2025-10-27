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
	"strings"
	"time"

	"github.com/pjscruggs/slogcp"
	"github.com/pjscruggs/slogcp/chatter"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

const unaryServerInstrumentation = "github.com/pjscruggs/slogcp/grpc/server"

// UnaryServerInterceptor returns a new unary server interceptor ([grpc.UnaryServerInterceptor])
// that logs incoming gRPC calls using the provided [slog.Logger].
//
// It automatically logs the gRPC service and method name, call duration, final
// gRPC status code, peer address, and any error returned by the handler upon
// completion of the RPC. Trace context is extracted from the incoming context
// for correlation in Cloud Trace (handled by the slogcp.Handler). It also recovers
// from panics in the handler, logs the panic with a stack trace, and returns a
// codes.Internal error.
//
// If configured via [WithMetadataLogging](true), it logs filtered incoming request
// metadata. Note that outgoing headers/trailers set via [grpc.SetHeader] or
// [grpc.SetTrailer] are not captured by this interceptor.
//
// If configured via [WithPayloadLogging](true), it logs the incoming request and
// outgoing response message payloads at [slog.LevelDebug]. Payload size can be limited
// using [WithMaxPayloadSize].
//
// Behavior can be further customized using [Option] functions (e.g., [WithLevels],
// [WithShouldLog], [WithMetadataFilter]). By default, all calls are logged,
// metadata and payloads are not logged, and a standard mapping from gRPC status
// codes to log levels is used.
func UnaryServerInterceptor(logger *slog.Logger, opts ...Option) grpc.UnaryServerInterceptor {
	// Process the provided options to get the final configuration.
	cfg := processOptions(opts...)

	cfg.configLogOnce.Do(func() {
		emitChatterConfig(logger, cfg.chatterEngine)
	})

	// Return the actual interceptor function, closing over logger and config.
	return func(
		ctx context.Context, // Incoming context from the application.
		req interface{}, // Request message.
		info *grpc.UnaryServerInfo, // Info about the RPC.
		handler grpc.UnaryHandler, // The next interceptor or the final RPC handler.
	) (resp interface{}, err error) { // Named return value for easier access in final logging.

		// Extract inbound trace context from gRPC metadata using the global OTel propagator.
		// Fallback to X-Cloud-Trace-Context if no valid span was found.
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
			tracer = otel.Tracer(unaryServerInstrumentation)
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

		var chatterDecision *chatter.Decision
		if engine := cfg.chatterEngine; engine != nil {
			var metadataMap map[string][]string
			if len(incomingMD) > 0 {
				metadataMap = metadataToHeader(incomingMD)
			}
			chatterDecision = engine.EvaluateGRPC(info.FullMethod, metadataMap, peerAddr)
			if chatterDecision != nil && chatterDecision.Matched {
				ctx = chatter.ContextWithDecision(ctx, chatterDecision)
			}
		}

		// Check if this call should be logged based on the filter function and chatter decision.
		shouldLog := cfg.shouldLogFunc(ctx, info.FullMethod)
		forcedLog := chatterDecision != nil && chatterDecision.ForceLog
		if chatterDecision != nil && !chatterDecision.ShouldLog() && !forcedLog {
			shouldLog = false
		}
		if forcedLog {
			shouldLog = true
		}
		if !shouldLog {
			return handler(ctx, req)
		}

		// Record start time and split method name.
		startTime := time.Now()
		serviceName, methodName := splitMethodName(info.FullMethod)

		activeLogger := logger
		if cfg.attachLogger {
			reqLogger := logger
			if attrs, ok := slogcp.TraceAttributes(ctx, cfg.traceProjectID); ok {
				reqLogger = loggerWithAttrs(reqLogger, attrs)
			}
			ctx = slogcp.ContextWithLogger(ctx, reqLogger)
			activeLogger = reqLogger
		}

		// Prepare slice to hold metadata attributes for the final log.
		var metadataAttrs []slog.Attr
		if cfg.logMetadata {
			// Extract and filter incoming metadata.
			incomingMD, _ := metadata.FromIncomingContext(ctx)
			filteredRequestMD := filterMetadata(incomingMD, cfg.metadataFilterFunc)
			if filteredRequestMD != nil {
				metadataAttrs = append(metadataAttrs, slog.Group(grpcRequestMetadataKey, slog.Any(metadataValuesKey, filteredRequestMD)))
			}
		}

		// Log the "start" event.
		// Pass the original context (ctx) so the handler can extract trace info.
		startLevel := slog.LevelInfo
		startAttrs := []slog.Attr{
			slog.String(grpcServiceKey, serviceName),
			slog.String(grpcMethodKey, methodName),
			slog.String(peerAddressKey, peerAddr),
		}
		if cfg.logCategory != "" {
			startAttrs = append(startAttrs, slog.String(categoryKey, cfg.logCategory))
		}
		activeLogger.LogAttrs(ctx, startLevel, "Starting gRPC call", startAttrs...)

		// Log request payload if enabled.
		if cfg.logPayloads {
			logPayload(ctx, activeLogger, cfg, "received", req)
		}

		// Setup panic recovery and final logging. This defer runs after the handler returns or panics.
		defer func() {
			// Handle potential panics from the handler.
			// Pass logger and context to handlePanic for immediate logging.
			isPanic, panicErr := handlePanic(ctx, activeLogger, recover()) // Pass embedded *slog.Logger
			if isPanic {
				err = panicErr // Overwrite handler error with internal error on panic.
				resp = nil     // Ensure response is nil on panic.
			}

			// Calculate duration and determine final status/level.
			duration := time.Since(startTime)
			code := status.Code(err)
			if cfg.chatterEngine != nil && chatterDecision != nil {
				cfg.chatterEngine.FinalizeGRPC(chatterDecision, code.String(), duration, code == codes.OK)
			}

			level := cfg.levelFunc(code)
			if isPanic {
				level = internalLevelCritical // Override level for panics.
			}
			if chatterDecision != nil {
				switch strings.ToUpper(chatterDecision.SeverityHint()) {
				case "WARN":
					if level < slog.LevelWarn {
						level = slog.LevelWarn
					}
				case "ERROR":
					if level < slog.LevelError {
						level = slog.LevelError
					}
				}
			}

			// Assemble final log attributes using helpers.
			finishAttrs := assembleFinishAttrs(duration, err, peerAddr)
			// Trace attributes are handled automatically by the slogcp.Handler via context.
			// Panic attributes are logged directly in handlePanic.

			// Combine all attribute slices.
			logAttrs := make([]slog.Attr, 0, 4+len(metadataAttrs)) // Pre-allocate slice
			logAttrs = append(logAttrs, slog.String(grpcServiceKey, serviceName))
			logAttrs = append(logAttrs, slog.String(grpcMethodKey, methodName))
			if cfg.logCategory != "" {
				logAttrs = append(logAttrs, slog.String(categoryKey, cfg.logCategory))
			}
			logAttrs = append(logAttrs, finishAttrs...)
			logAttrs = append(logAttrs, metadataAttrs...)
			logAttrs = appendChatterAnnotations(logAttrs, chatterDecision, cfg.auditKeys)

			logMsg := "Finished gRPC call"
			// Panic message is logged separately in handlePanic.
			// We still log the finish event, but the error reflects the panic.
			if isPanic {
				logMsg = "Finished gRPC call after panic recovery"
			}
			activeLogger.LogAttrs(ctx, level, logMsg, logAttrs...)

		}() // End of defer function for panic recovery and logging.

		// Call the actual gRPC handler.
		// If a panic occurs here, the defer func above will recover, log, and set 'err'/'resp'.
		// Otherwise, 'resp' and 'err' will be the values returned normally by the handler.
		resp, err = handler(ctx, req)

		// Log response payload only on success and if enabled.
		// This check happens *before* the defer logs the final status,
		// using the 'err' returned directly from the handler.
		if err == nil && cfg.logPayloads {
			logPayload(ctx, activeLogger, cfg, "sent", resp)
		}

		// Return the response and error (potentially modified by the defer func).
		return resp, err
	}
}
