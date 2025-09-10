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

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/pjscruggs/slogcp"
)

// wrappedClientStream wraps an existing grpc.ClientStream to provide logging.
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

// NewStreamClientInterceptor returns a new streaming client interceptor.
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

		// Prepare outgoing metadata, optionally injecting trace headers.
		originalOutgoingMD, _ := metadata.FromOutgoingContext(ctx)
		ctxWithOutgoingMD := ctx
		if cfg.propagateTraceHeaders {
			outgoingMDWithTrace := originalOutgoingMD.Copy()
			// Avoid duplicating injection if headers already exist.
			if len(outgoingMDWithTrace.Get(traceparentHeader)) == 0 {
				otel.GetTextMapPropagator().Inject(ctx, metadataCarrier{md: outgoingMDWithTrace})
			}
			// Also synthesize X-Cloud-Trace-Context if not present and we have a valid span.
			if len(outgoingMDWithTrace.Get(xCloudTraceContextHeaderMD)) == 0 {
				if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
					if v := formatXCloudTraceContextFromSpanContext(sc); v != "" {
						outgoingMDWithTrace[xCloudTraceContextHeaderMD] = []string{v}
					}
				}
			}
			ctxWithOutgoingMD = metadata.NewOutgoingContext(ctx, outgoingMDWithTrace)
		}

		// Filter outgoing metadata *before* calling streamer if logging is enabled.
		var filteredOutgoingMD metadata.MD
		if cfg.logMetadata {
			filteredOutgoingMD = filterMetadata(originalOutgoingMD, cfg.metadataFilterFunc)
		}

		// Log the "start" event.
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

			logger.LogAttrs(ctx, level, "Failed to start gRPC client stream", logAttrs...)
			return nil, err
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
			outgoingMD:   filteredOutgoingMD, // Store the pre-filtered outgoing MD.
		}

		return wrappedStream, nil
	}
}

// Header calls the underlying ClientStream's Header method, stores the result, and logs it.
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
			w.logger.LogAttrs(w.ctx, slog.LevelDebug, "gRPC client response header", logAttrs...)
		}
	}

	// If Header() returned an error, the stream is likely broken. Trigger finish.
	if err != nil {
		w.finishOnce.Do(func() { w.finish(err) })
	}

	return hdr, err
}

// Trailer stores the trailer and returns it. Trailer logging occurs in finish().
func (w *wrappedClientStream) Trailer() metadata.MD {
	trl := w.ClientStream.Trailer()
	w.mu.Lock()
	w.trailerMD = trl // Store trailer.
	w.mu.Unlock()
	return trl
}

// CloseSend calls the underlying ClientStream's CloseSend method.
func (w *wrappedClientStream) CloseSend() error {
	return w.ClientStream.CloseSend()
}

// Context returns the original context associated with the stream.
func (w *wrappedClientStream) Context() context.Context {
	return w.ctx
}

// SendMsg logs the outgoing message payload if configured, then calls SendMsg.
func (w *wrappedClientStream) SendMsg(m any) error {
	if w.opts.logPayloads {
		logPayload(w.ctx, w.logger, w.opts, "sent", m)
	}
	err := w.ClientStream.SendMsg(m)
	// Errors from SendMsg often indicate a broken stream. Trigger finish.
	if err != nil {
		w.finishOnce.Do(func() { w.finish(err) })
	}
	return err
}

// RecvMsg calls RecvMsg; on error (including io.EOF) it triggers final logging.
func (w *wrappedClientStream) RecvMsg(m any) error {
	err := w.ClientStream.RecvMsg(m)

	if err != nil {
		// Any error, including io.EOF, signifies the end or failure of the stream.
		w.finishOnce.Do(func() { w.finish(err) })
		return err
	}

	// Log payload only on successful receive.
	if w.opts.logPayloads {
		logPayload(w.ctx, w.logger, w.opts, "received", m)
	}

	return nil
}

// finish logs the completion event for the gRPC stream.
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

	// Assemble base attributes for the final log entry.
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
