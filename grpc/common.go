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
	"crypto/rand"
	"encoding/binary"
	"log/slog"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/pjscruggs/slogcp"
	"go.opentelemetry.io/otel/trace"
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

// HTTP/gRPC propagation header keys (metadata is always lowercase in gRPC).
const (
	traceparentHeader          = "traceparent"
	tracestateHeader           = "tracestate"
	xCloudTraceContextHeaderMD = "x-cloud-trace-context" // gRPC metadata key form
)

// splitMethodName parses a gRPC full method name into service and method components.
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
func defaultMetadataFilter(key string) bool {
	switch strings.ToLower(key) {
	case "authorization", "cookie", "set-cookie", "x-csrf-token", "grpc-trace-bin":
		return false // Exclude sensitive headers
	default:
		return true // Include all other headers
	}
}

// metadataToHeader converts gRPC metadata into a case-insensitive map suitable
// for reuse by shared helpers such as the health-check filter.
func metadataToHeader(md metadata.MD) map[string][]string {
	if len(md) == 0 {
		return nil
	}
	out := make(map[string][]string, len(md))
	for k, v := range md {
		lower := strings.ToLower(k)
		out[lower] = append([]string(nil), v...)
	}
	return out
}

// filterMetadata applies a filtering function to gRPC metadata.
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
		attrs = append(attrs, slog.Any("error", err))
	}
	return attrs
}

// handlePanic recovers from a panic during a gRPC call, logs details, and returns an error.
func handlePanic(ctx context.Context, logger *slog.Logger, recoveredValue any) (isPanic bool, err error) {
	if recoveredValue == nil {
		return false, nil // No panic occurred
	}

	isPanic = true

	stackStr, _ := slogcp.CaptureStack(nil)

	// Log the panic immediately with all available details
	logger.LogAttrs(ctx, internalLevelCritical,
		"Recovered panic during gRPC call",
		slog.Any(panicValueKey, recoveredValue),
		slog.String(panicStackKey, stackStr),
	)

	// Return a sanitized error to the client that doesn't expose internal details
	err = status.Errorf(codes.Internal, "internal server error caused by panic")
	return isPanic, err
}

// Internal level constant used for panic logging.
const (
	internalLevelCritical slog.Level = 12 // Corresponds to slogcp.LevelCritical
)

// metadataCarrier adapts gRPC metadata.MD to OTel's TextMapCarrier.
type metadataCarrier struct{ md metadata.MD }

// Get fetches the first metadata value for the provided key, returning an empty
// string when no value is present.
func (c metadataCarrier) Get(key string) string {
	// gRPC metadata is case-insensitive but normalized to lowercase keys.
	vals := c.md.Get(strings.ToLower(key))
	if len(vals) > 0 {
		return vals[0]
	}
	return ""
}

// Set replaces any existing values for key with a single entry containing
// value.
func (c metadataCarrier) Set(key string, value string) {
	lk := strings.ToLower(key)
	if c.md == nil {
		c.md = metadata.MD{}
	}
	// Replace any existing values for deterministic behavior.
	c.md[lk] = []string{value}
}

// Keys returns the set of metadata keys currently stored in the carrier.
func (c metadataCarrier) Keys() []string {
	keys := make([]string, 0, len(c.md))
	for k := range c.md {
		keys = append(keys, k)
	}
	return keys
}

// injectTraceContextFromXCloudHeader parses an X-Cloud-Trace-Context header value
// and returns a new context carrying a remote SpanContext. Invalid inputs return
// the original context unchanged.
//
// Header format: TRACE_ID/SPAN_ID;o=OPTIONS
//   - TRACE_ID: 32-char lowercase hex
//   - SPAN_ID: decimal uint64 (converted to hex bytes for OTel)
//   - OPTIONS: contains "o=1" if sampled (otherwise unsampled)
func injectTraceContextFromXCloudHeader(ctx context.Context, header string) context.Context {
	parts := strings.Split(header, "/")
	if len(parts) != 2 {
		return ctx
	}
	traceIDStr := parts[0]
	spanPart := parts[1]
	options := ""
	if i := strings.Index(spanPart, ";"); i >= 0 {
		options = spanPart[i+1:]
		spanPart = spanPart[:i]
	}

	tid, err := trace.TraceIDFromHex(traceIDStr)
	if err != nil || !tid.IsValid() {
		return ctx
	}

	// Parse decimal span id to bytes
	var sid trace.SpanID
	if spanPart != "" {
		if u, err := strconv.ParseUint(spanPart, 10, 64); err == nil {
			binary.BigEndian.PutUint64(sid[:], u)
		}
	}
	if !sid.IsValid() {
		// Generate a span id if input invalid
		if _, err := rand.Read(sid[:]); err != nil {
			return ctx
		}
	}

	var flags trace.TraceFlags
	if strings.Contains(options, "o=1") {
		flags = trace.FlagsSampled
	}

	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    tid,
		SpanID:     sid,
		TraceFlags: flags,
		Remote:     true,
	})
	return trace.ContextWithRemoteSpanContext(ctx, sc)
}

// formatXCloudTraceContextFromSpanContext builds the X-Cloud-Trace-Context value
// from a SpanContext: "<traceID-hex>/<spanID-dec>;o=<0|1>".
// Returns empty string if the span context is invalid.
func formatXCloudTraceContextFromSpanContext(sc trace.SpanContext) string {
	if !sc.IsValid() {
		return ""
	}
	// Convert span ID (8 bytes) to decimal uint64 using big-endian.
	sid := sc.SpanID() // value, not addressable; copy to local
	u := binary.BigEndian.Uint64(sid[:])
	o := "0"
	if sc.IsSampled() {
		o = "1"
	}
	return sc.TraceID().String() + "/" + strconv.FormatUint(u, 10) + ";o=" + o
}

// loggerWithAttrs returns a logger preconfigured with the supplied attributes
// when any are present.
func loggerWithAttrs(logger *slog.Logger, attrs []slog.Attr) *slog.Logger {
	if logger == nil || len(attrs) == 0 {
		return logger
	}
	args := make([]any, len(attrs))
	for i, attr := range attrs {
		args[i] = attr
	}
	return logger.With(args...)
}
