package grpc

import (
	"context"

	"log/slog"
	"path"
	"runtime"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Constants defining attribute keys used within this package for logging
// gRPC call details. These keys provide structure to the log entries.
const (
	// Attribute keys used for logging general gRPC call details.
	grpcServiceKey  = "grpc.service"  // Full gRPC service name (e.g., package.MyService).
	grpcMethodKey   = "grpc.method"   // gRPC method name (e.g., MyMethod).
	grpcCodeKey     = "grpc.code"     // String representation of the final gRPC status code (e.g., "OK", "Internal").
	grpcDurationKey = "grpc.duration" // Duration of the RPC call.
	peerAddressKey  = "peer.address"  // Client's network address (from server interceptor).

	// Attribute keys used for logging gRPC metadata (if enabled).
	grpcRequestMetadataKey = "grpc.request.metadata" // Key for request metadata group (server incoming, client outgoing).
	grpcResponseHeaderKey  = "grpc.response.header"  // Key for response header group (client incoming).
	grpcResponseTrailerKey = "grpc.response.trailer" // Key for response trailer group (client incoming).
	metadataValuesKey      = "values"                // Key within metadata group for the actual MD map.

	// Attribute keys used specifically for panic recovery logging.
	panicValueKey = "panic.value" // Key for the value recovered from panic().
	panicStackKey = "panic.stack" // Key for the stack trace captured during panic recovery.

	// defaultPanicStackBufSize defines the default buffer size allocated for
	// capturing the stack trace during panic recovery.
	defaultPanicStackBufSize = 8192

	// Attribute keys used specifically for payload logging (in payload.go).
	payloadDirectionKey    = "grpc.payload.direction"     // "sent" or "received"
	payloadTypeKey         = "grpc.payload.type"          // Go type of the payload message.
	payloadKey             = "grpc.payload.content"       // Key for the full payload content (as JSON string).
	payloadPreviewKey      = "grpc.payload.preview"       // Key for truncated payload content.
	payloadTruncatedKey    = "grpc.payload.truncated"     // Boolean indicating if payload was truncated.
	payloadOriginalSizeKey = "grpc.payload.original_size" // Original size before truncation.

	// categoryKey is used when WithLogCategory option is provided.
	categoryKey = "log.category"
)

// splitMethodName splits a full gRPC method name (e.g., "/package.Service/Method")
// into service and method parts.
func splitMethodName(fullMethodName string) (service, method string) {
	fullMethodName = strings.TrimPrefix(fullMethodName, "/")
	service = path.Dir(fullMethodName)
	method = path.Base(fullMethodName)

	// Represent root-level methods clearly.
	if service == "." || service == "" {
		service = "unknown"
	}
	return service, method
}

// defaultMetadataFilter provides a default deny-list for common sensitive headers.
// It returns true if the key should be logged, false otherwise.
// Filtering is case-insensitive.
func defaultMetadataFilter(key string) bool {
	switch strings.ToLower(key) {
	case "authorization", "cookie", "set-cookie", "x-csrf-token", "grpc-trace-bin":
		return false // Deny common sensitive headers and binary trace data.
	default:
		return true // Allow other headers by default.
	}
}

// filterMetadata applies the filter function to the metadata, returning a new
// MD containing only the keys for which the filter returned true.
// Returns nil if the input MD is nil/empty or if filtering results in an empty MD.
func filterMetadata(md metadata.MD, filterFunc MetadataFilterFunc) metadata.MD {
	if filterFunc == nil {
		filterFunc = defaultMetadataFilter
	}
	if len(md) == 0 {
		return nil
	}
	filtered := metadata.MD{}
	for k, v := range md {
		if filterFunc(k) {
			// Copy the slice to prevent aliasing issues.
			valsCopy := make([]string, len(v))
			copy(valsCopy, v)
			filtered[k] = valsCopy
		}
	}
	if len(filtered) == 0 {
		return nil
	}
	return filtered
}

// assembleFinishAttrs creates common attributes for the "finish" log event.
// peerAddr should be empty for client-side logs.
// It uses the string "error" for the error attribute key, allowing the slogcp.Handler
// to detect and potentially add stack traces automatically based on configuration.
func assembleFinishAttrs(duration time.Duration, err error, peerAddr string) []slog.Attr {
	grpcStatus := status.Code(err)
	attrs := []slog.Attr{
		slog.Duration(grpcDurationKey, duration),
		slog.String(grpcCodeKey, grpcStatus.String()),
	}
	if peerAddr != "" {
		attrs = append(attrs, slog.String(peerAddressKey, peerAddr))
	}
	if err != nil {
		// Use the standard "error" key. The slogcp handler will recognize this
		// and apply special formatting (like stack traces) if configured.
		attrs = append(attrs, slog.Any("error", err))
	}
	return attrs
}

// handlePanic recovers from a panic, logs the panic details immediately using
// the provided logger and context, captures the stack trace, and returns
// a standard codes.Internal error.
// It's intended for use in server interceptor defer functions.
func handlePanic(ctx context.Context, logger *slog.Logger, recoveredValue any) (isPanic bool, err error) {
	if recoveredValue == nil {
		return false, nil // No panic occurred.
	}

	isPanic = true
	stackBuf := make([]byte, defaultPanicStackBufSize)
	stackBytesWritten := runtime.Stack(stackBuf, false) // Capture stack trace.

	// Log the panic immediately at CRITICAL level using the provided logger and context.
	// The slogcp.Handler will automatically handle trace correlation from the context
	// and potentially format the panic value/stack according to its rules.
	logger.LogAttrs(ctx, internalLevelCritical, // Use internal critical level.
		"Recovered panic during gRPC call",
		slog.Any(panicValueKey, recoveredValue),
		slog.String(panicStackKey, string(stackBuf[:stackBytesWritten])),
	)

	// Return a standard Internal error to the client.
	err = status.Errorf(codes.Internal, "internal server error caused by panic")
	return isPanic, err
}

// These are needed within this package to avoid import cycles if slogcp.Level
// constants were used directly, especially in handlePanic.
const (
	internalLevelCritical slog.Level = 12 // Corresponds to slogcp.LevelCritical.
)
