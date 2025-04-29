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
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/pjscruggs/slogcp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// wrappedClientStream wraps an existing grpc.ClientStream to provide logging
// capabilities via slogcp. It intercepts key stream operations like SendMsg,
// RecvMsg, Header, and Trailer to log events, payloads (optionally), metadata
// (optionally), and the final stream status.
//
// It ensures that the final stream completion status is logged exactly once.
type wrappedClientStream struct {
	grpc.ClientStream // Embed the original stream interface.

	// Immutable fields set on creation
	logger    *slogcp.Logger  // Logger instance for recording events.
	opts      *options        // Processed configuration options for logging behavior.
	ctx       context.Context // Original context, retaining trace information.
	startTime time.Time       // Timestamp when the interceptor was invoked.
	service   string          // Parsed gRPC service name.
	method    string          // Parsed gRPC method name.

	// Mutable fields protected by mu
	mu           sync.Mutex
	finishOnce   sync.Once   // Ensures finish() runs only once.
	finished     bool        // Indicates if finish() has been executed.
	finalStatus  error       // Stores the terminal error/status of the stream.
	outgoingMD   metadata.MD // Filtered outgoing metadata (set during creation if logging enabled).
	headerMD     metadata.MD // Stores headers received via Header() call.
	trailerMD    metadata.MD // Stores trailers received via Trailer() call.
	headerLogged bool        // Tracks if headers have already been logged.
}

// NewStreamClientInterceptor returns a new streaming client interceptor (grpc.StreamClientInterceptor)
// that logs outgoing gRPC streams using the provided slogcp.Logger.
//
// It logs stream initiation details, completion status, duration, and optionally
// metadata and message payloads. Trace context from the incoming context.Context
// is automatically propagated in outgoing metadata and included in log entries
// by the slogcp.Handler.
//
// If configured via [WithMetadataLogging](true), it logs filtered outgoing request
// metadata and incoming response headers and trailers.
//
// If configured via [WithPayloadLogging](true), it logs individual messages sent
// and received on the stream at [slog.LevelDebug]. Payload size can be limited
// using [WithMaxPayloadSize].
//
// The behavior can be customized using functional options like [WithLevels],
// [WithShouldLog], [WithMetadataFilter].
func NewStreamClientInterceptor(logger *slogcp.Logger, opts ...Option) grpc.StreamClientInterceptor {
	cfg := processOptions(opts...) // Process configuration options.

	return func(
		ctx context.Context,
		desc *grpc.StreamDesc,
		cc *grpc.ClientConn,
		method string,
		streamer grpc.Streamer, // The next handler in the chain (interceptor or final streamer).
		callOpts ...grpc.CallOption,
	) (grpc.ClientStream, error) {

		// Apply filtering logic early.
		if !cfg.shouldLogFunc(ctx, method) {
			return streamer(ctx, desc, cc, method, callOpts...)
		}

		startTime := time.Now()
		serviceName, methodName := splitMethodName(method)

		// Prepare outgoing metadata with trace context.
		propagator := otel.GetTextMapPropagator()
		carrier := propagation.HeaderCarrier{}
		originalOutgoingMD, _ := metadata.FromOutgoingContext(ctx)
		outgoingMDWithTrace := originalOutgoingMD.Copy()
		propagator.Inject(ctx, carrier)
		for key, values := range carrier {
			if len(values) > 0 {
				outgoingMDWithTrace.Append(key, values...)
			}
		}
		ctxWithOutgoingMD := metadata.NewOutgoingContext(ctx, outgoingMDWithTrace)

		// Filter outgoing metadata *before* calling streamer if logging is enabled.
		var filteredOutgoingMD metadata.MD
		if cfg.logMetadata {
			filteredOutgoingMD = filterMetadata(originalOutgoingMD, cfg.metadataFilterFunc)
		}

		// Log the "start" event.
		// Pass the original context (ctx) so the handler can extract trace info.
		startAttrs := []slog.Attr{
			slog.String(grpcServiceKey, serviceName),
			slog.String(grpcMethodKey, methodName),
		}
		if cfg.logCategory != "" {
			startAttrs = append(startAttrs, slog.String(categoryKey, cfg.logCategory))
		}
		logger.LogAttrs(ctx, slog.LevelInfo, "Starting gRPC client stream", startAttrs...)

		// Call the next streamer in the chain using the context with propagated trace.
		clientStream, err := streamer(ctxWithOutgoingMD, desc, cc, method, callOpts...)

		// Handle errors during stream creation.
		if err != nil {
			duration := time.Since(startTime)
			grpcStatus := status.Code(err)
			level := cfg.levelFunc(grpcStatus)

			// Assemble attributes for the failure log.
			finishAttrs := assembleFinishAttrs(duration, err, "") // No peer addr for client
			// Trace attributes are handled automatically by the slogcp.Handler via context.
			var metadataAttrs []slog.Attr
			if filteredOutgoingMD != nil {
				metadataAttrs = append(metadataAttrs, slog.Group(grpcRequestMetadataKey, slog.Any(metadataValuesKey, filteredOutgoingMD)))
			}

			// Combine all attribute slices.
			logAttrs := make([]slog.Attr, 0, 3+len(metadataAttrs)) // Pre-allocate slice
			logAttrs = append(logAttrs, slog.String(grpcServiceKey, serviceName))
			logAttrs = append(logAttrs, slog.String(grpcMethodKey, methodName))
			if cfg.logCategory != "" {
				logAttrs = append(logAttrs, slog.String(categoryKey, cfg.logCategory))
			}
			logAttrs = append(logAttrs, finishAttrs...)
			logAttrs = append(logAttrs, metadataAttrs...)

			// Log using the original context (ctx).
			logger.LogAttrs(ctx, level, "Failed to start gRPC client stream", logAttrs...)
			return nil, err // Return the original error.
		}

		// Stream created successfully, wrap it for logging.
		wrappedStream := &wrappedClientStream{
			ClientStream: clientStream,
			logger:       logger,
			opts:         cfg,
			ctx:          ctx, // Store original context for trace info in finish().
			startTime:    startTime,
			service:      serviceName,
			method:       methodName,
			// projectID is not needed here, handler gets it from context/config.
			outgoingMD: filteredOutgoingMD, // Store the pre-filtered outgoing MD.
		}

		return wrappedStream, nil
	}
}

// Header calls the underlying ClientStream's Header method, stores the result,
// logs the header metadata if configured (and not already logged), and returns
// the result. If an error occurs, it triggers the final stream logging.
func (w *wrappedClientStream) Header() (metadata.MD, error) {
	hdr, err := w.ClientStream.Header()

	w.mu.Lock()
	w.headerMD = hdr // Store header regardless of error or nilness.
	logNow := w.opts.logMetadata && !w.headerLogged && err == nil && hdr != nil
	if logNow {
		w.headerLogged = true // Mark as logged *within* the lock.
	}
	w.mu.Unlock()

	// Perform logging outside the lock if needed.
	if logNow {
		filteredHeaderMD := filterMetadata(hdr, w.opts.metadataFilterFunc)
		if filteredHeaderMD != nil {
			// Log using the original context for trace correlation.
			logAttrs := []slog.Attr{
				slog.Group(grpcResponseHeaderKey, slog.Any(metadataValuesKey, filteredHeaderMD)),
				slog.String(grpcServiceKey, w.service),
				slog.String(grpcMethodKey, w.method),
			}
			if w.opts.logCategory != "" {
				logAttrs = append(logAttrs, slog.String(categoryKey, w.opts.logCategory))
			}
			// Trace info is added automatically by the handler from w.ctx.
			w.logger.LogAttrs(w.ctx, slog.LevelDebug, "gRPC client response header", logAttrs...)
		}
	}

	// If Header() returned an error, the stream is likely broken. Trigger finish.
	if err != nil {
		w.finishOnce.Do(func() { w.finish(err) })
	}

	return hdr, err
}

// Trailer calls the underlying ClientStream's Trailer method, stores the result,
// and returns it. Trailer logging occurs in the finish() method.
func (w *wrappedClientStream) Trailer() metadata.MD {
	trl := w.ClientStream.Trailer()
	w.mu.Lock()
	w.trailerMD = trl // Store trailer.
	w.mu.Unlock()
	return trl
}

// CloseSend calls the underlying ClientStream's CloseSend method.
// It does not trigger the finish log, as the stream is still open for receiving.
func (w *wrappedClientStream) CloseSend() error {
	return w.ClientStream.CloseSend()
}

// Context returns the original context associated with the stream,
// preserving trace information for logging.
func (w *wrappedClientStream) Context() context.Context {
	return w.ctx
}

// SendMsg logs the outgoing message payload if configured, then calls the
// underlying ClientStream's SendMsg method. If SendMsg returns an error,
// it triggers the final stream logging.
func (w *wrappedClientStream) SendMsg(m any) error {
	if w.opts.logPayloads {
		// Log using the original context (w.ctx).
		logPayload(w.ctx, w.logger, w.opts, "sent", m)
	}
	err := w.ClientStream.SendMsg(m)
	// Errors from SendMsg often indicate a broken stream. Trigger finish.
	if err != nil {
		w.finishOnce.Do(func() { w.finish(err) })
	}
	return err
}

// RecvMsg calls the underlying ClientStream's RecvMsg method. If an error
// (including io.EOF) occurs, it triggers the final stream logging via finish().
// If the receive is successful and payload logging is enabled, it logs the payload.
func (w *wrappedClientStream) RecvMsg(m any) error {
	err := w.ClientStream.RecvMsg(m)

	if err != nil {
		// Any error, including io.EOF, signifies the end or failure of the stream.
		// Trigger the final logging exactly once.
		w.finishOnce.Do(func() { w.finish(err) })
		return err // Return the original error.
	}

	// Log payload only on successful receive.
	if w.opts.logPayloads {
		// Log using the original context (w.ctx).
		logPayload(w.ctx, w.logger, w.opts, "received", m)
	}

	return nil // Successful receive.
}

// finish logs the completion event for the gRPC stream. It calculates the
// duration, determines the final status and log level, logs metadata if configured,
// and emits the final log record using the original context.
// This method is designed to be called exactly once via finishOnce.Do().
func (w *wrappedClientStream) finish(err error) {
	w.mu.Lock()
	if w.finished {
		w.mu.Unlock()
		return
	}
	w.finished = true
	w.finalStatus = err // Store the triggering error/status.
	hdr := w.headerMD
	trl := w.trailerMD
	outMD := w.outgoingMD // Already filtered during creation.
	headerAlreadyLogged := w.headerLogged
	w.mu.Unlock()

	duration := time.Since(w.startTime)

	// Determine final status code and log level.
	finalErr := err
	if finalErr == io.EOF { // Treat EOF from RecvMsg as success.
		finalErr = nil
	}
	level := w.opts.levelFunc(status.Code(finalErr))

	// Trace attributes are handled automatically by the slogcp.Handler via context.

	// Assemble base attributes for the final log entry using helper.
	finishAttrs := assembleFinishAttrs(duration, finalErr, "") // No peer addr for client

	// Prepare slice for metadata attributes.
	var metadataAttrs []slog.Attr

	// Add filtered metadata attributes if logging is enabled.
	if w.opts.logMetadata {
		if outMD != nil {
			metadataAttrs = append(metadataAttrs, slog.Group(grpcRequestMetadataKey, slog.Any(metadataValuesKey, outMD)))
		}
		if !headerAlreadyLogged && hdr != nil {
			filteredHeaderMD := filterMetadata(hdr, w.opts.metadataFilterFunc)
			if filteredHeaderMD != nil {
				metadataAttrs = append(metadataAttrs, slog.Group(grpcResponseHeaderKey, slog.Any(metadataValuesKey, filteredHeaderMD)))
			}
		}
		if trl != nil {
			filteredTrailerMD := filterMetadata(trl, w.opts.metadataFilterFunc)
			if filteredTrailerMD != nil {
				metadataAttrs = append(metadataAttrs, slog.Group(grpcResponseTrailerKey, slog.Any(metadataValuesKey, filteredTrailerMD)))
			}
		}
	}

	// Combine all attribute slices.
	logAttrs := make([]slog.Attr, 0, 3+len(metadataAttrs)) // Pre-allocate slice
	logAttrs = append(logAttrs, slog.String(grpcServiceKey, w.service))
	logAttrs = append(logAttrs, slog.String(grpcMethodKey, w.method))
	if w.opts.logCategory != "" {
		logAttrs = append(logAttrs, slog.String(categoryKey, w.opts.logCategory))
	}
	logAttrs = append(logAttrs, finishAttrs...)
	logAttrs = append(logAttrs, metadataAttrs...)

	// Log the final stream completion event using the original context (w.ctx).
	w.logger.LogAttrs(w.ctx, level, "Finished gRPC client stream", logAttrs...)
}
