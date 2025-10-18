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

package slogcp

import (
	"context"
	"io"
	"log/slog"
	"os"
	"time"

	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"

	"github.com/pjscruggs/slogcp/internal/gcp"
)

// LogTarget defines the destination for log output.
// It is an alias for the internal gcp.LogTarget type.
type LogTarget = gcp.LogTarget

const (
	// LogTargetGCP directs logs to the Google Cloud Logging API.
	LogTargetGCP LogTarget = gcp.LogTargetGCP

	// LogTargetStdout directs logs to standard output as structured JSON (default).
	// Use this for local development or when running in environments without
	// GCP integration.
	LogTargetStdout LogTarget = gcp.LogTargetStdout

	// LogTargetStderr directs logs to standard error as structured JSON.
	// Similar to LogTargetStdout but writes to stderr instead.
	LogTargetStderr LogTarget = gcp.LogTargetStderr

	// LogTargetFile directs logs to a specified file as structured JSON.
	// Requires a file path to be provided via WithRedirectToFile.
	LogTargetFile LogTarget = gcp.LogTargetFile
)

// Option configures a Logger during initialization via the New function.
// Options are applied sequentially, allowing later options to override earlier ones
// or settings derived from environment variables.
type Option func(*options)

// Middleware transforms the handler chain.
// Each middleware wraps the handler returned by the previous middleware or the base handler.
// This allows for custom processing of log records before they reach the final handler.
type Middleware func(slog.Handler) slog.Handler

// options holds the builder state for programmatic configuration.
// Fields are pointers to allow differentiating between an explicitly set
// zero value/nil/false and an unset option (which would then fall back to
// environment variables or defaults).
type options struct {
	// slogcp specific
	level             *slog.Level
	addSource         *bool
	stackTraceEnabled *bool
	stackTraceLevel   *slog.Level
	logTarget         *LogTarget
	redirectWriter    io.Writer // For WithRedirectWriter
	redirectFilePath  *string   // For WithRedirectToFile
	replaceAttr       func([]string, slog.Attr) slog.Attr
	middlewares       []Middleware
	programmaticAttrs []slog.Attr // Accumulates WithAttrs calls
	programmaticGroup *string     // Last WithGroup wins

	// GCP Client/Logger passthrough fields
	projectID                *string
	parent                   *string
	traceProjectID           *string
	gcpLogID                 *string
	clientScopes             []string // Use slice directly, nil means unset
	clientOnErrorFunc        func(error)
	programmaticCommonLabels map[string]string // Accumulates WithGCPCommonLabel/WithGCPCommonLabels
	gcpMonitoredResource     *mrpb.MonitoredResource
	gcpConcurrentWriteLimit  *int
	gcpDelayThreshold        *time.Duration
	gcpEntryCountThreshold   *int
	gcpEntryByteThreshold    *int
	gcpEntryByteLimit        *int
	gcpBufferedByteLimit     *int
	gcpContextFunc           func() (context.Context, func())
	gcpDefaultContextTimeout *time.Duration
	gcpPartialSuccess        *bool
}

// WithLevel returns an Option that sets the minimum logging level.
// Log records with a level lower than this will be discarded.
// This overrides the LOG_LEVEL environment variable.
func WithLevel(level slog.Level) Option { return func(o *options) { o.level = &level } }

// WithSourceLocationEnabled returns an Option that enables or disables
// including source code location (file, line, function) in log entries.
// Enabling this helps with debugging but adds overhead to logging operations.
// This overrides the LOG_SOURCE_LOCATION environment variable.
func WithSourceLocationEnabled(enabled bool) Option {
	return func(o *options) { o.addSource = &enabled }
}

// WithStackTraceEnabled returns an Option that enables or disables automatic
// stack trace capture for logs at or above the configured stack trace level.
// When enabled, errors logged at or above the stack trace level include stack traces,
// which helps with debugging but increases log entry size.
// This overrides the LOG_STACK_TRACE_ENABLED environment variable.
func WithStackTraceEnabled(enabled bool) Option {
	return func(o *options) { o.stackTraceEnabled = &enabled }
}

// WithStackTraceLevel returns an Option that sets the minimum level for stack traces.
// Stack traces are only included for log records at or above this level
// and only if stack trace capture is enabled.
// This overrides the LOG_STACK_TRACE_LEVEL environment variable.
func WithStackTraceLevel(level slog.Level) Option {
	return func(o *options) { o.stackTraceLevel = &level }
}

// WithLogTarget returns an Option that explicitly sets the logging destination.
// The log target determines where logs are sent: GCP, stdout, stderr, or a file.
// This overrides the SLOGCP_LOG_TARGET environment variable.
func WithLogTarget(target LogTarget) Option { return func(o *options) { o.logTarget = &target } }

// WithReplaceAttr returns an Option that sets a function to replace or modify
// attributes before they are logged. This allows for filtering sensitive information,
// transforming values, or adding additional attributes based on existing ones.
// The function receives the current group path and the attribute to process.
func WithReplaceAttr(fn func([]string, slog.Attr) slog.Attr) Option {
	return func(o *options) { o.replaceAttr = fn }
}

// WithMiddleware registers a middleware that decorates the Handler. Middlewares
// are applied in the order they are provided, wrapping the handler sequentially.
// Each middleware can modify log records, filter them, or add additional processing.
// Nil middlewares are silently ignored.
func WithMiddleware(mw Middleware) Option {
	return func(o *options) {
		if mw != nil {
			o.middlewares = append(o.middlewares, mw)
		}
	}
}

// WithAttrs returns an Option that adds attributes to be included with every log record.
// These attributes are added before any record-specific attributes.
// Multiple calls to WithAttrs are cumulative.
// Empty attribute slices are ignored.
func WithAttrs(attrs []slog.Attr) Option {
	return func(o *options) {
		if len(attrs) == 0 {
			return
		}
		// Copy to prevent modification by caller.
		attrsCopy := make([]slog.Attr, len(attrs))
		copy(attrsCopy, attrs)
		o.programmaticAttrs = append(o.programmaticAttrs, attrsCopy...)
	}
}

// WithGroup returns an Option that sets an initial group namespace for the logger.
// All attributes added to the logger will be placed under this group.
// The last call to WithGroup overrides any previous calls.
func WithGroup(name string) Option { return func(o *options) { o.programmaticGroup = &name } }

// WithRedirectToStdout configures logging to standard output.
// This sets the LogTarget to LogTargetStdout and configures an appropriate writer.
// It's useful for local development or containerized environments where
// stdout is captured by a logging system.
func WithRedirectToStdout() Option {
	return func(o *options) {
		target := gcp.LogTargetStdout
		o.logTarget = &target
		o.redirectWriter = os.Stdout
		o.redirectFilePath = nil // Clear file path if set previously
	}
}

// WithRedirectToStderr configures logging to standard error.
// This sets the LogTarget to LogTargetStderr and configures an appropriate writer.
// It's useful when you want to separate logs from normal program output
// or when stdout is used for other purposes.
func WithRedirectToStderr() Option {
	return func(o *options) {
		target := gcp.LogTargetStderr
		o.logTarget = &target
		o.redirectWriter = os.Stderr
		o.redirectFilePath = nil // Clear file path
	}
}

// WithRedirectToFile configures logging to a specified file path.
// This sets the LogTarget to LogTargetFile. The file will be opened in append mode.
// The logger's Close method must be called to close the file and prevent resource leaks.
// If the file doesn't exist, it will be created. If it exists, logs will be appended.
func WithRedirectToFile(filePath string) Option {
	return func(o *options) {
		target := gcp.LogTargetFile
		o.logTarget = &target
		o.redirectFilePath = &filePath
		o.redirectWriter = nil // Clear explicit writer if set previously
	}
}

// WithRedirectWriter configures logging to a custom io.Writer.
// The caller is responsible for the lifecycle of the provided writer.
// However, slogcp.Logger.Close will attempt to call Close on the writer
// if it implements io.Closer.
//
// This option is suitable for integrating with self-rotating log writers,
// such as "gopkg.in/natefinch/lumberjack.v2". For example:
//
//	lj := &lumberjack.Logger{ /* ... lumberjack config ... */ }
//	logger, err := slogcp.New(slogcp.WithRedirectWriter(lj))
//	// ...
//	defer logger.Close() // This will call lj.Close()
//
// Using WithRedirectWriter implies a non-GCP log target. The logger.New function
// determines the specific target based on the writer type and other configurations.
// This option typically overrides file path-based redirection if both are specified.
func WithRedirectWriter(writer io.Writer) Option {
	return func(o *options) {
		o.redirectWriter = writer
		o.redirectFilePath = nil // Clear file path
		// Do not force LogTarget here; let logger.New resolve conflicts/defaults.
	}
}

// WithProjectID returns an Option that explicitly sets the Google Cloud Project ID.
// This is required when using LogTargetGCP and no project ID is available from
// environment variables or metadata.
// This overrides GOOGLE_CLOUD_PROJECT and metadata server detection.
// It also influences the default Parent if Parent is not explicitly set.
func WithProjectID(id string) Option { return func(o *options) { o.projectID = &id } }

// WithParent returns an Option that explicitly sets the GCP resource parent
// (e.g., "projects/P", "folders/F", "organizations/O", "billingAccounts/B").
// This controls where logs are stored in the GCP resource hierarchy.
// This overrides SLOGCP_GCP_PARENT and any parent derived from ProjectID.
func WithParent(parent string) Option { return func(o *options) { o.parent = &parent } }

// WithTraceProjectID returns an Option that sets the project ID used for
// formatting trace resource names in log entries. If not set, the main
// ProjectID is used. This overrides the SLOGCP_TRACE_PROJECT_ID environment
// variable.
func WithTraceProjectID(id string) Option { return func(o *options) { o.traceProjectID = &id } }

// WithGCPLogID returns an Option that explicitly sets the Cloud Logging log ID.
// The log ID identifies the specific log within Cloud Logging where entries are written.
// If not set, the default log ID "app" is used.
// This overrides SLOGCP_GCP_LOG_ID and any default value.
func WithGCPLogID(logID string) Option { return func(o *options) { o.gcpLogID = &logID } }

// WithGCPClientScopes returns an Option that sets the OAuth2 scopes for the GCP client.
// This is typically only needed when using custom authentication methods or
// when additional permissions beyond the default logging scopes are required.
// This overrides the default logging scopes.
// Passing an empty list sets the scopes to nil, reverting to defaults.
func WithGCPClientScopes(scopes ...string) Option {
	return func(o *options) {
		if len(scopes) == 0 {
			o.clientScopes = nil
			return
		}
		sCopy := make([]string, len(scopes))
		copy(sCopy, scopes)
		o.clientScopes = sCopy
	}
}

// WithGCPClientOnError returns an Option that sets a custom error handler for
// background errors encountered by the GCP logging client.
// These errors occur asynchronously during batch processing and
// would otherwise be reported to stderr.
func WithGCPClientOnError(f func(error)) Option { return func(o *options) { o.clientOnErrorFunc = f } }

// WithGCPCommonLabel returns an Option that adds a single common label to be included
// with log entries sent to GCP. Labels help organize and filter logs in the Cloud Console.
// Multiple calls are cumulative and override any matching labels from environment variables.
func WithGCPCommonLabel(key, value string) Option {
	return func(o *options) {
		if o.programmaticCommonLabels == nil {
			o.programmaticCommonLabels = make(map[string]string)
		}
		o.programmaticCommonLabels[key] = value
	}
}

// WithGCPCommonLabels returns an Option that sets the entire map of common labels.
// Labels help organize and filter logs in the Cloud Console.
// This replaces any labels set by environment variables or previous calls to
// WithGCPCommonLabel / WithGCPCommonLabels.
// Passing an empty map clears all programmatically set labels.
func WithGCPCommonLabels(labels map[string]string) Option {
	return func(o *options) {
		if len(labels) == 0 {
			o.programmaticCommonLabels = nil // Allow clearing
			return
		}
		lCopy := make(map[string]string, len(labels))
		for k, v := range labels {
			lCopy[k] = v
		}
		o.programmaticCommonLabels = lCopy // Replace previous programmatic map
	}
}

// WithGCPMonitoredResource returns an Option that explicitly sets the MonitoredResource
// for log entries sent to GCP. This controls how logs are categorized and displayed
// in the Cloud Console.
// This overrides environment variables and auto-detection that normally determine
// the resource type (e.g., GCE instance, GKE container, Cloud Run service).
func WithGCPMonitoredResource(res *mrpb.MonitoredResource) Option {
	return func(o *options) { o.gcpMonitoredResource = res }
}

// WithGCPConcurrentWriteLimit returns an Option that sets the number of goroutines
// used by the GCP client for sending log entries.
// Higher values may improve throughput at the cost of more resource usage.
// Default is 1 (serial sending).
func WithGCPConcurrentWriteLimit(n int) Option {
	return func(o *options) { o.gcpConcurrentWriteLimit = &n }
}

// WithGCPDelayThreshold returns an Option that sets the maximum time the GCP client
// buffers entries before sending them to Cloud Logging.
// Longer delays increase batching efficiency but add latency to log delivery.
// Default is 1 second.
func WithGCPDelayThreshold(d time.Duration) Option {
	return func(o *options) { o.gcpDelayThreshold = &d }
}

// WithGCPEntryCountThreshold returns an Option that sets the maximum number of entries
// the GCP client buffers before sending them to Cloud Logging.
// Higher values improve batching efficiency but increase memory usage.
// Default is 1000 entries.
func WithGCPEntryCountThreshold(n int) Option {
	return func(o *options) { o.gcpEntryCountThreshold = &n }
}

// WithGCPEntryByteThreshold returns an Option that sets the maximum size of entries
// the GCP client buffers before sending them to Cloud Logging.
// Higher values improve batching efficiency but increase memory usage.
// Default is 8 MiB.
func WithGCPEntryByteThreshold(n int) Option {
	return func(o *options) { o.gcpEntryByteThreshold = &n }
}

// WithGCPEntryByteLimit returns an Option that sets the maximum size of a single
// log entry that can be sent by the GCP client.
// Entries larger than this are truncated.
// Default is 0 (no limit).
func WithGCPEntryByteLimit(n int) Option { return func(o *options) { o.gcpEntryByteLimit = &n } }

// WithGCPBufferedByteLimit returns an Option that sets the total memory limit
// for buffered log entries in the GCP client.
// If this limit is reached, new log entries may be dropped until the buffer drains.
// Overrides slogcp default of 100 MiB.
func WithGCPBufferedByteLimit(n int) Option { return func(o *options) { o.gcpBufferedByteLimit = &n } }

// WithGCPContextFunc returns an Option that provides a function to generate
// contexts for background GCP client operations.
// This allows for custom timeouts, cancellation, or propagation of values
// to API calls made by the logging client.
func WithGCPContextFunc(f func() (context.Context, func())) Option {
	return func(o *options) { o.gcpContextFunc = f }
}

// WithGCPDefaultContextTimeout returns an Option that sets a timeout for the default
// context function used by the GCP client if WithGCPContextFunc is not provided.
// This helps prevent API calls from hanging indefinitely.
// The default behavior is to use context.Background() with no timeout.
func WithGCPDefaultContextTimeout(d time.Duration) Option {
	return func(o *options) { o.gcpDefaultContextTimeout = &d }
}

// WithGCPPartialSuccess returns an Option that enables the partial success flag
// when writing log entries to the GCP API.
// When enabled, the API will process as many log entries as possible in a batch,
// even if some entries are invalid.
// When disabled (default), the entire batch fails if any entry is invalid.
func WithGCPPartialSuccess(enable bool) Option {
	return func(o *options) { o.gcpPartialSuccess = &enable }
}
