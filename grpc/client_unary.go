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

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// NewUnaryClientInterceptor creates a gRPC unary client interceptor that logs RPC calls
// using the provided slog logger and configuration options.
func NewUnaryClientInterceptor(logger *slog.Logger, opts ...Option) grpc.UnaryClientInterceptor {
	// Process the provided options to get the final configuration.
	cfg := processOptions(opts...)

	// Return the actual interceptor function, closing over logger and config.
	return func(
		ctx context.Context, // Incoming context from the application.
		method string, // Full gRPC method string, e.g., "/package.Service/Method".
		req, reply any, // Request and reply message pointers.
		cc *grpc.ClientConn, // Client connection used for the call.
		invoker grpc.UnaryInvoker, // The next interceptor or the final gRPC invocation logic.
		callOpts ...grpc.CallOption, // Per-call options from the application.
	) (err error) { // Named return error for easier access in final logging.

		// Check if this call should be logged based on the filter function.
		if !cfg.shouldLogFunc(ctx, method) {
			return invoker(ctx, method, req, reply, cc, callOpts...)
		}

		// Record start time and split method name.
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
			// Optionally synthesize X-Cloud-Trace-Context if requested.
			if cfg.injectLegacyXCloud && len(outgoingMDWithTrace.Get(xCloudTraceContextHeaderMD)) == 0 {
				if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
					if v := formatXCloudTraceContextFromSpanContext(sc); v != "" {
						outgoingMDWithTrace[xCloudTraceContextHeaderMD] = []string{v}
					}
				}
			}
			ctxWithOutgoingMD = metadata.NewOutgoingContext(ctx, outgoingMDWithTrace)
		}

		// Prepare slice to hold metadata attributes for the final log.
		var metadataAttrs []slog.Attr

		// Log Request Metadata (if enabled).
		if cfg.logMetadata {
			filteredRequestMD := filterMetadata(originalOutgoingMD, cfg.metadataFilterFunc)
			if filteredRequestMD != nil {
				metadataAttrs = append(metadataAttrs,
					slog.Group(grpcRequestMetadataKey, slog.Any(metadataValuesKey, filteredRequestMD)),
				)
			}
		}

		// Log the "start" event.
		startAttrs := []slog.Attr{
			slog.String(grpcServiceKey, serviceName),
			slog.String(grpcMethodKey, methodName),
		}
		if cfg.logCategory != "" {
			startAttrs = append(startAttrs, slog.String(categoryKey, cfg.logCategory))
		}
		logger.LogAttrs(ctx, slog.LevelInfo, "Starting gRPC client call", startAttrs...)

		// Log Request Payload (if enabled).
		if cfg.logPayloads {
			logPayload(ctx, logger, cfg, "sent", req)
		}

		// Prepare to Capture Response Metadata.
		var headerMD, trailerMD metadata.MD
		headerCallOpt := grpc.Header(&headerMD)
		trailerCallOpt := grpc.Trailer(&trailerMD)

		// Combine original call options with our metadata capture options.
		finalCallOpts := append([]grpc.CallOption{headerCallOpt, trailerCallOpt}, callOpts...)

		// Setup final logging using defer. This runs after the invoker returns.
		defer func() {
			duration := time.Since(startTime)
			level := cfg.levelFunc(status.Code(err)) // Use the named return error 'err'.

			// Log Response Metadata (if enabled).
			if cfg.logMetadata {
				filteredHeaderMD := filterMetadata(headerMD, cfg.metadataFilterFunc)
				if filteredHeaderMD != nil {
					metadataAttrs = append(metadataAttrs,
						slog.Group(grpcResponseHeaderKey, slog.Any(metadataValuesKey, filteredHeaderMD)),
					)
				}
				filteredTrailerMD := filterMetadata(trailerMD, cfg.metadataFilterFunc)
				if filteredTrailerMD != nil {
					metadataAttrs = append(metadataAttrs,
						slog.Group(grpcResponseTrailerKey, slog.Any(metadataValuesKey, filteredTrailerMD)),
					)
				}
			}

			// Assemble Final Log Attributes using helpers.
			finishAttrs := assembleFinishAttrs(duration, err, "") // No peer address for client

			// Combine all attribute slices.
			logAttrs := make([]slog.Attr, 0, 3+len(metadataAttrs)) // Pre-allocate slice
			logAttrs = append(logAttrs, slog.String(grpcServiceKey, serviceName))
			logAttrs = append(logAttrs, slog.String(grpcMethodKey, methodName))
			if cfg.logCategory != "" {
				logAttrs = append(logAttrs, slog.String(categoryKey, cfg.logCategory))
			}
			logAttrs = append(logAttrs, finishAttrs...)
			logAttrs = append(logAttrs, metadataAttrs...)

			// Log Completion Message using the original context (ctx).
			logger.LogAttrs(ctx, level, "Finished gRPC client call", logAttrs...)
		}() // End of defer function for final logging.

		// Invoke the RPC using the context with propagated trace headers.
		err = invoker(ctxWithOutgoingMD, method, req, reply, cc, finalCallOpts...)

		// Log Response Payload (if enabled and successful).
		if err == nil && cfg.logPayloads {
			logPayload(ctx, logger, cfg, "received", reply)
		}

		return err
	}
}
