package grpc

import (
	"context"
	"fmt"
	"hash/fnv"
	"log/slog"
	"math"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
)

// Option is a function type used to configure gRPC interceptors created by
// this package, such as [UnaryServerInterceptor] and [StreamServerInterceptor].
// It follows the functional options pattern.
type Option func(*options)

// CodeToLevel is a function type that maps a gRPC status code to an slog.Level.
// This function is used by the interceptor to determine the severity level
// of the final log message emitted when an RPC call finishes, based on the
// call's gRPC status code. A default mapping is provided if [WithLevels] is not used.
type CodeToLevel func(code codes.Code) slog.Level

// ShouldLogFunc is a function type that determines whether a given gRPC call
// should be logged based on its context and full method name
// (e.g., "/package.Service/Method").
//
// Returning true indicates the call should be logged (both start/finish events
// and potentially payloads if enabled); returning false skips all logging for
// that specific call. This is useful for filtering out health checks or other
// high-volume, low-interest RPCs. A default function that always returns true
// is used if [WithShouldLog] is not provided.
type ShouldLogFunc func(ctx context.Context, fullMethodName string) bool

// MetadataFilterFunc defines the signature for functions used to filter
// metadata keys before logging. It should return true to keep the key,
// false to discard it. Keys are passed in their original case as received
// from the gRPC library, but filtering logic should typically be case-insensitive.
type MetadataFilterFunc func(key string) bool

// options holds the configuration settings for the gRPC logging interceptors.
// This struct is internal to the grpc package and is populated by applying
// the public Option functions during interceptor creation.
type options struct {
	levelFunc          CodeToLevel        // Function to map gRPC code to slog level for the final log entry.
	shouldLogFunc      ShouldLogFunc      // Function to filter which calls get logged.
	logPayloads        bool               // Flag to enable request/response payload logging.
	maxPayloadLogSize  int                // Max size in bytes for logged payloads (0 means no limit).
	logMetadata        bool               // Flag to enable request/response metadata logging.
	metadataFilterFunc MetadataFilterFunc // Function to filter metadata keys before logging.
	skipPaths          []string           // Method paths to exclude from logging (e.g., health checks).
	samplingRate       float64            // 0.0-1.0, percentage of requests to log.
	logCategory        string             // Optional category name to distinguish logs.
}

const (
	// defaultMaxPayloadLogSize defines the default behavior for payload size logging,
	// which is no limit (log the full payload if payload logging is enabled).
	defaultMaxPayloadLogSize = 0
	// defaultLogCategory defines the default category attribute value for gRPC logs.
	defaultLogCategory = "grpc_request"
)

// WithSkipPaths returns an Option that excludes specific method paths from logging.
// This is useful for health checks or other high-volume, low-value endpoints.
// Each path string is checked for containment in the full method name.
func WithSkipPaths(paths []string) Option {
	return func(o *options) {
		// Create a copy to avoid modifying the caller's slice.
		o.skipPaths = append([]string(nil), paths...)
	}
}

// WithSamplingRate returns an Option that logs only a percentage of requests.
// Rate should be between 0.0 (log none) and 1.0 (log all).
// Sampling is deterministic based on the method name and timestamp for distribution.
func WithSamplingRate(rate float64) Option {
	return func(o *options) {
		// Clamp the rate between 0.0 and 1.0.
		if rate < 0 {
			rate = 0
		}
		if rate > 1 {
			rate = 1
		}
		o.samplingRate = rate
	}
}

// WithLogCategory returns an Option that adds a category field to distinguish
// these logs from other log entries (including platform-generated requests).
// The category is included as an attribute in log entries.
func WithLogCategory(category string) Option {
	return func(o *options) {
		o.logCategory = category
	}
}

// defaultCodeToLevel provides a sensible default mapping from gRPC status codes
// to slog severity levels. This aims to categorize common errors appropriately.
func defaultCodeToLevel(code codes.Code) slog.Level {
	switch code {
	case codes.OK:
		return slog.LevelInfo // Successful calls are informational.
	case codes.Canceled:
		return slog.LevelInfo // Cancellations are often expected, log as Info.
	case codes.InvalidArgument, codes.NotFound, codes.AlreadyExists, codes.Unauthenticated, codes.PermissionDenied:
		return slog.LevelWarn // Client errors or auth issues are typically Warnings.
	case codes.DeadlineExceeded, codes.ResourceExhausted, codes.FailedPrecondition, codes.Aborted, codes.OutOfRange, codes.Unavailable:
		return slog.LevelWarn // Server-side issues that might be transient or retryable are Warnings.
	case codes.Unknown, codes.Unimplemented, codes.Internal, codes.DataLoss:
		return slog.LevelError // Clear server-side failures or data issues are Errors.
	default:
		// Treat any unrecognized gRPC code as an Error by default.
		return slog.LevelError
	}
}

// defaultShouldLog provides the default behavior for call filtering, which is to log every call.
func defaultShouldLog(_ context.Context, _ string) bool {
	return true
}

// WithLevels returns an Option that sets the function used to map gRPC status codes
// to slog log levels. This mapping determines the severity of the log entry emitted
// when an RPC call completes.
//
// If f is nil or this option is not provided, a default mapping (suitable for
// typical server-side error categorization) is used. See [CodeToLevel] for the
// function signature.
func WithLevels(f CodeToLevel) Option {
	return func(o *options) {
		if f != nil {
			o.levelFunc = f
		} else {
			// Explicitly reset to default if nil is passed.
			o.levelFunc = defaultCodeToLevel
		}
	}
}

// WithShouldLog returns an Option that sets a filter function to dynamically
// determine whether a specific gRPC call should be logged. The function f receives
// the call's context and full method name (e.g., "/package.Service/Method").
// If f returns false, the interceptor will skip all logging actions (start,
// finish, payload, metadata) for that call.
//
// If f is nil or this option is not provided, all calls are logged by default.
// See [ShouldLogFunc] for the function signature.
func WithShouldLog(f ShouldLogFunc) Option {
	return func(o *options) {
		if f != nil {
			o.shouldLogFunc = f
		} else {
			// Reset to default if nil is passed.
			o.shouldLogFunc = defaultShouldLog
		}
	}
}

// WithPayloadLogging returns an Option that enables or disables the logging of
// gRPC request and response message payloads. This is primarily intended for
// debugging, especially with streaming RPCs, as it can generate significant log volume
// and potentially expose sensitive data.
//
// Payload logging is disabled by default. When enabled, payloads are logged at
// [slog.LevelDebug]. Use [WithMaxPayloadSize] to control the size of logged payloads.
func WithPayloadLogging(enabled bool) Option {
	return func(o *options) {
		o.logPayloads = enabled
	}
}

// WithMaxPayloadSize returns an Option that sets the maximum size (in bytes)
// for logged message payloads when payload logging is enabled via [WithPayloadLogging](true).
//
// If a marshalled payload exceeds sizeBytes, it will be truncated in the log output,
// and attributes indicating truncation will be added. A value of 0 or less
// signifies that no size limit should be applied (payloads logged fully).
// Defaults to 0 (no limit). This option has no effect if payload logging is disabled.
func WithMaxPayloadSize(sizeBytes int) Option {
	return func(o *options) {
		if sizeBytes <= 0 {
			o.maxPayloadLogSize = 0 // Treat non-positive as no limit
		} else {
			o.maxPayloadLogSize = sizeBytes
		}
	}
}

// WithMetadataLogging returns an Option that enables or disables the logging
// of gRPC request and response metadata (headers/trailers).
// Metadata logging is disabled by default. When enabled, metadata keys are
// filtered using the configured MetadataFilterFunc (see WithMetadataFilter).
// Logged metadata is typically placed under keys like "grpc.request.metadata".
func WithMetadataLogging(enabled bool) Option {
	return func(o *options) {
		o.logMetadata = enabled
	}
}

// WithMetadataFilter returns an Option that sets a custom function to filter
// metadata keys before they are logged. The function should return true for
// keys to be included in the logs. This option only has an effect if metadata
// logging is enabled via [WithMetadataLogging](true).
//
// If f is nil or this option is not provided, a default filter is used which
// excludes common sensitive headers like "Authorization" and "Cookie".
// See [MetadataFilterFunc] for the function signature.
func WithMetadataFilter(f MetadataFilterFunc) Option {
	return func(o *options) {
		if f != nil {
			o.metadataFilterFunc = f
		} else {
			// Reset to default if nil is passed.
			o.metadataFilterFunc = defaultMetadataFilter
		}
	}
}

// processOptions creates a new internal options struct initialized with default values,
// then iterates through the provided Option functions, applying each one to potentially
// override the defaults. It returns the final configured options struct, including
// a composite shouldLogFunc that incorporates skipPaths and sampling.
func processOptions(opts ...Option) *options {
	// Start with a struct populated with default settings.
	opt := &options{
		levelFunc:          defaultCodeToLevel,
		shouldLogFunc:      defaultShouldLog,
		logPayloads:        false,
		maxPayloadLogSize:  defaultMaxPayloadLogSize,
		logMetadata:        false,
		metadataFilterFunc: defaultMetadataFilter,
		skipPaths:          nil,
		samplingRate:       1.0, // Default: log all requests
		logCategory:        defaultLogCategory,
	}

	// Apply each provided Option function to modify the defaults.
	for _, o := range opts {
		if o != nil { // Guard against nil options in the slice
			o(opt)
		}
	}

	// Store the original user-provided shouldLogFunc for composition.
	originalShouldLog := opt.shouldLogFunc

	// Create a new composite shouldLogFunc that incorporates skipPaths and sampling.
	// This ensures all filtering logic is applied in a consistent order.
	opt.shouldLogFunc = func(ctx context.Context, fullMethodName string) bool {
		// 1. Apply the original user-provided filter first.
		if !originalShouldLog(ctx, fullMethodName) {
			return false
		}

		// 2. Check against skipPaths.
		for _, skipPath := range opt.skipPaths {
			// Check if the method name contains the path to skip.
			if skipPath != "" && strings.Contains(fullMethodName, skipPath) {
				return false
			}
		}

		// 3. Apply sampling if the rate is less than 100%.
		// Handle 0.0 rate explicitly.
		if opt.samplingRate <= 0.0 {
			return false // Log none if rate is 0 or less.
		}
		if opt.samplingRate < 1.0 {
			// Use a deterministic hash based on method name and timestamp for distribution.
			// This avoids global state and provides stable sampling per method over time.
			h := fnv.New32a()
			_, _ = h.Write([]byte(fullMethodName)) // Hash method name.

			// Include nanosecond timestamp for randomization across calls.
			timeNs := time.Now().UnixNano()
			_, _ = h.Write([]byte(fmt.Sprintf("%d", timeNs)))

			// Convert hash to a float between 0.0 and 1.0.
			hashFloat := float64(h.Sum32()) / float64(math.MaxUint32)

			// Skip logging if the hash falls outside the sampling rate.
			if hashFloat >= opt.samplingRate {
				return false
			}
		}

		// If all checks passed, the request should be logged.
		return true
	}

	return opt
}
