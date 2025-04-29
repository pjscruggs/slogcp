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

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/pjscruggs/slogcp"
)

// wrappedServerStream wraps an existing grpc.ServerStream to intercept SendMsg
// and RecvMsg calls. This allows for optional logging of message payloads
// without modifying the core interceptor logic significantly. It also ensures
// the correct context, potentially modified by preceding interceptors, is
// consistently returned by the Context() method.
type wrappedServerStream struct {
	grpc.ServerStream
	ctx    context.Context // The context associated with this stream, potentially modified by interceptors.
	logger *slogcp.Logger  // The logger instance used for payload logging.
	opts   *options        // Configuration options determining logging behavior (e.g., payload logging enabled).
}

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
// that logs incoming gRPC streams using the provided [slogcp.Logger].
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
func StreamServerInterceptor(logger *slogcp.Logger, opts ...Option) grpc.StreamServerInterceptor {
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

		// Check if this call should be logged based on the filter function.
		if !cfg.shouldLogFunc(ctx, info.FullMethod) {
			// If not logging, call the handler directly and return.
			return handler(srv, ss)
		}

		// Extract initial info for logging.
		peerAddr := "unknown"
		if p, ok := peer.FromContext(ctx); ok {
			peerAddr = p.Addr.String()
		}
		serviceName, methodName := splitMethodName(info.FullMethod)

		// Prepare metadata attribute slice early if needed for the final log.
		var metadataAttrs []slog.Attr
		if cfg.logMetadata {
			// Extract and filter incoming metadata *before* calling the handler.
			incomingMD, _ := metadata.FromIncomingContext(ctx)
			filteredIncomingMD := filterMetadata(incomingMD, cfg.metadataFilterFunc)
			if filteredIncomingMD != nil {
				metadataAttrs = append(metadataAttrs, slog.Group(grpcRequestMetadataKey, slog.Any(metadataValuesKey, filteredIncomingMD)))
			}
		}

		// Log the "start" event.
		// Pass the original context (ctx) so the handler can extract trace info.
		startAttrs := []slog.Attr{
			slog.String(grpcServiceKey, serviceName),
			slog.String(grpcMethodKey, methodName),
			slog.String(peerAddressKey, peerAddr),
		}
		if cfg.logCategory != "" {
			startAttrs = append(startAttrs, slog.String(categoryKey, cfg.logCategory))
		}
		logger.LogAttrs(ctx, slog.LevelInfo, "Starting gRPC stream", startAttrs...)

		// Setup panic recovery and final logging. This defer runs after the handler returns or panics.
		defer func() {
			// Handle potential panics from the handler.
			// Pass logger and context to handlePanic for immediate logging.
			// handlePanic uses the logger directly, which uses the context-aware handler.
			isPanic, panicErr := handlePanic(ctx, logger.Logger, recover()) // Pass embedded *slog.Logger
			if isPanic {
				err = panicErr // Overwrite handler error with internal error on panic.
			}

			// Calculate duration and determine final status/level.
			duration := time.Since(startTime)
			level := cfg.levelFunc(status.Code(err)) // Determine level based on final error.
			if isPanic {
				level = internalLevelCritical // Override level for panics.
			}

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

			logMsg := "Finished gRPC stream"
			// Panic message is logged separately in handlePanic.
			// We still log the finish event, but the error reflects the panic.
			if isPanic {
				logMsg = "Finished gRPC stream after panic recovery"
			}
			// Log the final completion event using the original context (ctx).
			// The handler will extract trace info from ctx.
			logger.LogAttrs(ctx, level, logMsg, logAttrs...)

		}() // End of defer function for panic recovery and logging.

		// Wrap the stream if payload logging is enabled, otherwise use the original stream.
		// This allows intercepting SendMsg/RecvMsg for logging payloads.
		var streamToUse grpc.ServerStream = ss
		if cfg.logPayloads {
			streamToUse = &wrappedServerStream{
				ServerStream: ss,
				// Pass the original context (ctx) to the wrapper.
				// This context will be used by logPayload calls inside the wrapper.
				ctx:    ctx,
				logger: logger,
				opts:   cfg,
			}
		}

		// Call the actual gRPC stream handler with the (potentially wrapped) stream.
		// If a panic occurs here, the defer func above will recover, log, and set 'err'.
		// Otherwise, 'err' will be the error returned normally by the handler.
		err = handler(srv, streamToUse)

		// Return the error (either from the handler or from panic recovery).
		return err
	}
}
