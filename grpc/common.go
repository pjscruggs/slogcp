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
	"path"
	"runtime"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Attribute keys used within gRPC interceptors to structure log entries.
// These keys form a consistent schema for logs in both client and server
// interceptors, making it easier to query and analyze gRPC traffic patterns.
const (
	// Core gRPC call identification and results
	grpcServiceKey  = "grpc.service"  // Service name from the full method path (e.g., "myapp.UserService")
	grpcMethodKey   = "grpc.method"   // Method name from the full path (e.g., "GetUser")
	grpcCodeKey     = "grpc.code"     // Final status code as a string (e.g., "OK", "NotFound")
	grpcDurationKey = "grpc.duration" // Total call duration as a time.Duration
	peerAddressKey  = "peer.address"  // Remote endpoint address (IP:port or UDS path)

	// Metadata logging keys - used when metadata logging is enabled
	grpcRequestMetadataKey = "grpc.request.metadata" // Contains filtered request headers
	grpcResponseHeaderKey  = "grpc.response.header"  // Contains filtered response headers
	grpcResponseTrailerKey = "grpc.response.trailer" // Contains filtered trailers
	metadataValuesKey      = "values"                // Used within metadata groups to hold the actual values

	// Panic recovery logging - used when a handler panics
	panicValueKey = "panic.value"       // The value recovered from the panic
	panicStackKey = "panic.stack_trace" // Formatted stack trace from the panic site

	// Buffer size for stack trace capture during panic recovery
	// 8KB is typically sufficient for capturing the relevant part of deep stacks
	defaultPanicStackBufSize = 8192

	// Payload logging keys - used when payload logging is enabled
	// These keys are used in payload.go for request/response body logging
	payloadDirectionKey    = "grpc.payload.direction"     // Either "sent" or "received"
	payloadTypeKey         = "grpc.payload.type"          // Go type of the message (e.g., "*mypb.UserRequest")
	payloadKey             = "grpc.payload.content"       // Full JSON representation of non-truncated payload
	payloadPreviewKey      = "grpc.payload.preview"       // Truncated content when payload exceeds size limit
	payloadTruncatedKey    = "grpc.payload.truncated"     // Boolean flag indicating truncation
	payloadOriginalSizeKey = "grpc.payload.original_size" // Original size in bytes before truncation

	// Miscellaneous
	categoryKey = "log.category" // Optional grouping category from WithLogCategory
)

// splitMethodName parses a gRPC full method name into service and method components.
//
// gRPC method names are formatted as "/package.Service/Method". This function
// extracts the service part ("package.Service") and method part ("Method"),
// handling edge cases like missing slashes or empty components.
//
// For example:
//   - "/users.UserService/GetUser" → "users.UserService", "GetUser"
//   - "users.UserService/GetUser" → "users.UserService", "GetUser" (handles missing leading slash)
//   - "/GetUser" → "unknown", "GetUser" (handles missing service component)
//
// The service name is used for grouping related methods in logs, and the method name
// identifies the specific RPC operation being performed.
func splitMethodName(fullMethodName string) (service, method string) {
	fullMethodName = strings.TrimPrefix(fullMethodName, "/")
	service = path.Dir(fullMethodName)
	method = path.Base(fullMethodName)

	// Handle root-level methods (no service component)
	if service == "." || service == "" {
		service = "unknown"
	}
	return service, method
}

// defaultMetadataFilter provides basic filtering of sensitive metadata.
// It returns true for keys that should be included in logs, false for keys
// that should be excluded for security or privacy reasons.
//
// This filter blocks common authentication and security headers:
//   - "authorization": Contains credentials, tokens, or passwords
//   - "cookie"/"set-cookie": May contain session tokens or other sensitive data
//   - "x-csrf-token": Security token for preventing CSRF attacks
//   - "grpc-trace-bin": Binary trace data that's handled separately by the tracer
//
// All other headers are allowed by default. This filter is case-insensitive
// to handle variations in header casing.
//
// Applications with stricter requirements should provide a custom filter
// using the WithMetadataFilter option.
func defaultMetadataFilter(key string) bool {
	switch strings.ToLower(key) {
	case "authorization", "cookie", "set-cookie", "x-csrf-token", "grpc-trace-bin":
		return false // Exclude sensitive headers
	default:
		return true // Include all other headers
	}
}

// filterMetadata applies a filtering function to gRPC metadata.
// It creates a new metadata.MD containing only the key-value pairs where
// the filter function returns true for the key.
//
// The filter is applied to each key in the original metadata. If the filter
// returns true, the values for that key are copied to a new slice to prevent
// aliasing issues, and added to the result.
//
// If filterFunc is nil, defaultMetadataFilter is used.
// If the input metadata is empty or all keys are filtered out, nil is returned.
//
// This function is used internally by the interceptors to safely log a subset
// of metadata without exposing sensitive values.
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
			// Deep copy the value slice to avoid aliasing with the original
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

// assembleFinishAttrs creates a standard set of attributes for RPC completion logs.
// This function centralizes the creation of common attributes used in the final
// log entry when a gRPC call completes, ensuring consistent structure across
// both client and server interceptors.
//
// Parameters:
//   - duration: The total time taken for the RPC call
//   - err: The error returned by the handler or received from the server
//   - peerAddr: The remote endpoint address (should be empty for client-side logs)
//
// The returned attributes include:
//   - grpc.duration: Call duration as a time.Duration
//   - grpc.code: Final status code as a string (from the error if present)
//   - peer.address: Client address (for server logs only)
//   - error: The original error object if one occurred
//
// The "error" attribute uses the standard key name, allowing the slogcp handler
// to apply special formatting and extract stack traces based on configuration.
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
		// Using the standard "error" key allows the slogcp handler to recognize this
		// as an error and potentially add stack traces automatically
		attrs = append(attrs, slog.Any("error", err))
	}
	return attrs
}

// handlePanic recovers from a panic during a gRPC call, logs detailed information,
// and returns an appropriate error to the client.
//
// When a panic occurs in a gRPC handler, this function:
// 1. Captures a stack trace at the point of the panic
// 2. Logs the panic value and stack trace at CRITICAL level
// 3. Returns a standard Internal error to the client
//
// The panic is logged immediately with details useful for debugging, but the client
// receives only a generic error message to avoid leaking implementation details.
// This maintains security while providing operators with the information they need
// to investigate the issue.
//
// The context is passed to logging to preserve trace correlation.
// The function is intended to be called from defer blocks in interceptors.
//
// Returns:
//   - isPanic: true if a panic was recovered, false otherwise
//   - err: a gRPC Internal error if a panic occurred, nil otherwise
func handlePanic(ctx context.Context, logger *slog.Logger, recoveredValue any) (isPanic bool, err error) {
	if recoveredValue == nil {
		return false, nil // No panic occurred
	}

	isPanic = true

	// Capture the stack trace at the point of panic
	stackBuf := make([]byte, defaultPanicStackBufSize)
	stackBytesWritten := runtime.Stack(stackBuf, false)

	// Log the panic immediately with all available details
	// The CRITICAL level ensures operators are alerted to the issue
	logger.LogAttrs(ctx, internalLevelCritical,
		"Recovered panic during gRPC call",
		slog.Any(panicValueKey, recoveredValue),
		slog.String(panicStackKey, string(stackBuf[:stackBytesWritten])),
	)

	// Return a sanitized error to the client that doesn't expose internal details
	err = status.Errorf(codes.Internal, "internal server error caused by panic")
	return isPanic, err
}

// Internal level constant used for panic logging.
// This avoids a package import cycle with the main slogcp package.
const (
	internalLevelCritical slog.Level = 12 // Corresponds to slogcp.LevelCritical
)
