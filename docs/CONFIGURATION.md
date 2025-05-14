# slogcp Configuration

This document provides a comprehensive guide to configuring the `slogcp` library and its subpackages for HTTP and gRPC logging.

## Overview

`slogcp` uses a layered configuration approach:
1.  **Defaults**: Sensible defaults are applied first.
2.  **Environment Variables**: Override defaults. These are loaded when `slogcp.New()` is called.
3.  **Programmatic Options**: Options passed to `slogcp.New(opts ...Option)` override both defaults and environment variables.

This allows for flexible configuration suitable for various deployment environments.

## Core Logger Configuration (`slogcp.New`)

These options configure the main `slogcp.Logger` instance.

### Log Level

Controls the minimum severity of logs that will be processed.

-   **Programmatic**: `slogcp.WithLevel(slog.Level)`
    -   Example: `slogcp.WithLevel(slogcp.LevelDebug)`
-   **Environment Variable**: `LOG_LEVEL`
    -   Values: `debug`, `info`, `warn`, `error`, `notice`, `critical`, `alert`, `emergency`, or numeric `slog.Level` values.
-   **Default**: `info` (`slog.LevelInfo`)

The logger's level can be changed dynamically after creation using `logger.SetLevel(slog.Level)`.

### Log Target

Determines where log entries are sent.

-   **Programmatic Options**:
    -   `slogcp.WithLogTarget(target slogcp.LogTarget)`: Explicitly sets the target (`LogTargetGCP`, `LogTargetStdout`, `LogTargetStderr`, `LogTargetFile`).
    -   `slogcp.WithRedirectToStdout()`: Convenience for `LogTargetStdout`.
    -   `slogcp.WithRedirectToStderr()`: Convenience for `LogTargetStderr`.
    -   `slogcp.WithRedirectToFile(filePath string)`: Convenience for `LogTargetFile`, logs to the specified path.
    -   `slogcp.WithRedirectWriter(writer io.Writer)`: Logs to a custom writer. `slogcp` will determine the target based on the writer or other options.
-   **Environment Variables**:
    -   `SLOGCP_LOG_TARGET`: Sets the general mode (`gcp`, `stdout`, `stderr`, `file`).
    -   `SLOGCP_REDIRECT_AS_JSON_TARGET`: Specifies the destination for non-GCP modes.
        -   `stdout`: Redirects to standard output.
        -   `stderr`: Redirects to standard error.
        -   `file:/path/to/your.log`: Redirects to the specified file.
-   **Default**: `gcp` (`slogcp.LogTargetGCP`)

**Target-Specific Notes:**

#### GCP Cloud Logging API (Default)
-   Requires GCP project information (see [GCP Project ID and Parent Resource](#gcp-project-id-and-parent-resource)).
-   Uses the `cloud.google.com/go/logging` client.
-   Log entries are sent to a log named `slogcp_application_logs`.

#### Standard Output (stdout)
-   Logs structured JSON to `os.Stdout`.
-   Useful for local development or containerized environments where logs are collected from stdout.

#### Standard Error (stderr)
-   Logs structured JSON to `os.Stderr`.

#### File
-   Logs structured JSON to a specified file.
-   If `slogcp` opens the file (via `WithRedirectToFile` or `SLOGCP_REDIRECT_AS_JSON_TARGET=file:...`), you can use `logger.ReopenLogFile()` to support external log rotation tools (like `logrotate`). This method closes the current file and reopens it at the original path.
-   The file is opened in append mode (`O_APPEND|O_CREATE|O_WRONLY`) with permissions `0644`.

#### Custom `io.Writer`
-   Use `slogcp.WithRedirectWriter(writer)` to log to any `io.Writer`.
-   This is ideal for integrating with libraries like `lumberjack` for self-rotating logs.
-   If the provided writer implements `io.Closer`, `logger.Close()` will call the writer's `Close()` method.

### Source Code Location

Includes the source file, line number, and function name in log entries.

-   **Programmatic**: `slogcp.WithSourceLocationEnabled(enabled bool)`
-   **Environment Variable**: `LOG_SOURCE_LOCATION` (values: `true`, `false`)
-   **Default**: `false` (disabled)
-   Enabling this adds some performance overhead.

### Stack Traces

Automatically captures stack traces for logs at or above a specified level, typically for errors.

-   **Programmatic**:
    -   `slogcp.WithStackTraceEnabled(enabled bool)`
    -   `slogcp.WithStackTraceLevel(level slog.Level)`
-   **Environment Variables**:
    -   `LOG_STACK_TRACE_ENABLED` (values: `true`, `false`)
    -   `LOG_STACK_TRACE_LEVEL` (values: `debug`, `info`, `error`, etc.)
-   **Defaults**:
    -   Enabled: `false`
    -   Level: `error` (`slog.LevelError`)
-   Stack traces are included in the `stack_trace` field for GCP Error Reporting compatibility.

### Attribute Replacement

Allows modification or removal of attributes before they are logged.

-   **Programmatic**: `slogcp.WithReplaceAttr(fn func(groups []string, attr slog.Attr) slog.Attr)`
    -   The function receives the list of current groups and the attribute. It should return the modified attribute. Return an empty `slog.Attr{}` to remove it.
-   **Environment Variable**: None.
-   **Default**: No replacement function.

### Handler Middleware

Applies custom transformations to the `slog.Handler` chain.

-   **Programmatic**: `slogcp.WithMiddleware(mw func(slog.Handler) slog.Handler)`
    -   Middlewares are applied in the order provided, each wrapping the previous one.
-   **Environment Variable**: None.
-   **Default**: No middlewares.

### Initial Attributes and Groups

Adds attributes or a group to all log records produced by the logger.

-   **Programmatic**:
    -   `slogcp.WithAttrs(attrs []slog.Attr)`: Adds attributes. Multiple calls are cumulative.
    -   `slogcp.WithGroup(name string)`: Sets an initial group. The last call wins.
-   **Environment Variable**: None.
-   **Default**: No initial attributes or group.

### GCP Project ID and Parent Resource

Required when `LogTarget` is `LogTargetGCP`. The Parent resource (e.g., `projects/PROJECT_ID`) determines where logs are written. The Project ID is also used for formatting trace IDs.

-   **Programmatic**:
    -   `slogcp.WithProjectID(id string)`: Sets the Project ID. Infers Parent as `projects/ID`.
    -   `slogcp.WithParent(parent string)`: Sets the Parent (e.g., `projects/ID`, `folders/ID`, `organizations/ID`, `billingAccounts/ID`). If project-based, infers Project ID.
-   **Environment Variables (in order of precedence for Project ID/Parent derivation)**:
    1.  `SLOGCP_GCP_PARENT`: Explicitly sets the parent. If project-based (e.g., `projects/my-gcp-project`), `my-gcp-project` is used as Project ID.
    2.  `SLOGCP_PROJECT_ID`: Sets the Project ID. Parent defaults to `projects/YOUR_SLOGCP_PROJECT_ID`.
    3.  `GOOGLE_CLOUD_PROJECT`: Standard GCP environment variable for Project ID. Parent defaults to `projects/YOUR_GOOGLE_CLOUD_PROJECT`.
-   **Metadata Server**: If running on GCP (e.g., GCE, GKE, Cloud Run) and no environment variables are set, the Project ID is auto-detected from the metadata server. Parent defaults to `projects/DETECTED_PROJECT_ID`.
-   **Default**: None. If `LogTargetGCP` is used and no Project ID/Parent can be resolved, `slogcp.New()` returns an error.

### Automatic Fallback Mode

If no log target is explicitly configured (via options or environment variables) and the default `LogTargetGCP` initialization fails (e.g., due to missing credentials or project ID locally), `slogcp.New()` will automatically fall back to structured JSON logging on `stdout`.

-   **Detection**: Use `logger.IsInFallbackMode() bool` to check if the logger is operating in this fallback mode.
-   This behavior simplifies local development, as the same code can log to GCP in production and `stdout` locally without changes.
-   If a target *is* explicitly configured, fallback does not occur, and initialization errors are returned.

### Closing the Logger

It's crucial to close the logger to ensure all buffered logs are flushed and resources are released.

-   **Method**: `logger.Close() error`
-   Call this method during application shutdown, typically using `defer logger.Close()`.
-   `Close()` is idempotent (safe to call multiple times).
-   Behavior:
    -   **GCP Mode**: Flushes logs to Cloud Logging and closes the client.
    -   **File Mode (slogcp-managed)**: Closes the log file opened by `slogcp`.
    -   **Custom `io.Writer` Mode**: If the writer implements `io.Closer`, its `Close()` method is called.

## GCP Client Options (for `LogTargetGCP`)

These options fine-tune the behavior of the underlying Google Cloud Logging client when `LogTarget` is `LogTargetGCP`. Many correspond to options in `cloud.google.com/go/logging.ClientOptions` or `logging.LoggerOptions`.

### Authentication Scopes

Specifies OAuth2 scopes for the GCP client.

-   **Programmatic**: `slogcp.WithGCPClientScopes(scopes ...string)`
-   **Environment Variable**: `SLOGCP_GCP_CLIENT_SCOPES` (comma-separated string)
-   **Default**: Uses the default scopes required for `cloud.google.com/go/logging` (typically `https://www.googleapis.com/auth/logging.write` and `https/www.googleapis.com/auth/cloud-platform`).

### Background Error Handling

Sets a custom error handler for asynchronous errors from the GCP client (e.g., during batch sends).

-   **Programmatic**: `slogcp.WithGCPClientOnError(f func(error))`
-   **Environment Variable**: None.
-   **Default**: Errors are logged to `os.Stderr`.

### Common Labels

Adds labels to all log entries sent to GCP. Useful for filtering and organization in Cloud Logging.

-   **Programmatic**:
    -   `slogcp.WithGCPCommonLabel(key, value string)`: Adds a single label. Cumulative.
    -   `slogcp.WithGCPCommonLabels(labels map[string]string)`: Sets the entire map of labels, replacing previous programmatic labels.
-   **Environment Variables**:
    -   `SLOGCP_GCP_COMMON_LABELS_JSON`: A JSON string representing a map of labels (e.g., `{"env":"prod","service":"api"}`).
    -   `SLOGCP_GCP_CL_*`: Individual labels via prefixed environment variables (e.g., `SLOGCP_GCP_CL_APP_VERSION=1.2.3`).
-   **Precedence**: Labels from `SLOGCP_GCP_CL_*` override those from `SLOGCP_GCP_COMMON_LABELS_JSON`. Programmatic options override environment variables.
-   **Default**: No common labels.

### Monitored Resource

Sets the `MonitoredResource` for log entries, associating them with specific GCP resources.

-   **Programmatic**: `slogcp.WithGCPMonitoredResource(res *mrpb.MonitoredResource)`
-   **Environment Variables**:
    -   `SLOGCP_GCP_RESOURCE_TYPE`: The type of the resource (e.g., `gce_instance`, `k8s_container`).
    -   `SLOGCP_GCP_RL_*`: Labels for the resource (e.g., `SLOGCP_GCP_RL_INSTANCE_ID=my-instance`, `SLOGCP_GCP_RL_ZONE=us-central1-a`).
-   **Default**: Auto-detected by the Cloud Logging client library based on the environment.

### Buffering and Batching

Controls how the GCP client buffers log entries before sending them in batches.

-   **`WithGCPConcurrentWriteLimit(n int)` / `SLOGCP_GCP_CONCURRENT_WRITE_LIMIT`**:
    Number of goroutines for sending log entries. Default: `1`.
-   **`WithGCPDelayThreshold(d time.Duration)` / `SLOGCP_GCP_DELAY_THRESHOLD_MS`**:
    Max time client buffers entries. Default: `1 second`.
-   **`WithGCPEntryCountThreshold(n int)` / `SLOGCP_GCP_ENTRY_COUNT_THRESHOLD`**:
    Max number of entries buffered. Default: `1000`.
-   **`WithGCPEntryByteThreshold(n int)` / `SLOGCP_GCP_ENTRY_BYTE_THRESHOLD`**:
    Max size of entries buffered (bytes). Default: `8 MiB` (8 * 1024 * 1024 bytes).
-   **`WithGCPEntryByteLimit(n int)` / `SLOGCP_GCP_ENTRY_BYTE_LIMIT`**:
    Max size of a single log entry (bytes). Larger entries are dropped or truncated by the client library. Default: `0` (no limit by `slogcp`, but GCP service limits apply, typically around 256KB).
-   **`WithGCPBufferedByteLimit(n int)` / `SLOGCP_GCP_BUFFERED_BYTE_LIMIT`**:
    Total memory limit for all buffered entries. If reached, new entries may be dropped. Default: `100 MiB` (100 * 1024 * 1024 bytes) - this is an `slogcp` default, overriding the GCP client library's own default if any.

### API Call Context

Customizes the `context.Context` used for background GCP client operations.

-   **Programmatic**: `slogcp.WithGCPContextFunc(f func() (context.Context, func()))`
    -   The function should return a context and a cancel function.
-   **`WithGCPDefaultContextTimeout(d time.Duration)` / `SLOGCP_GCP_CONTEXT_FUNC_TIMEOUT_MS`**:
    Sets a timeout for the default context function if `WithGCPContextFunc` is not used.
-   **Default**: `context.Background()` with no timeout for operations, though client initialization has a default timeout of `10 seconds`.

### Partial Success

Enables partial success for batch log writes to the GCP API.

-   **Programmatic**: `slogcp.WithGCPPartialSuccess(enable bool)`
-   **Environment Variable**: `SLOGCP_GCP_PARTIAL_SUCCESS` (values: `true`, `false`)
-   **Default**: `false` (the entire batch fails if any entry is invalid).

## Log Levels (`slogcp.Level`)

`slogcp` extends standard `slog.Level` to include all Google Cloud Logging severity levels. The `slogcp.Level` type is compatible with `slog.Level`.

| `slogcp.Level` Constant | Underlying `slog.Level` Value | GCP Severity String | Mapped `logging.Severity` |
|-------------------------|-------------------------------|---------------------|---------------------------|
| `slogcp.LevelDefault`   | -8                            | `DEFAULT`           | `logging.Default`         |
| `slogcp.LevelDebug`     | -4 (`slog.LevelDebug`)        | `DEBUG`             | `logging.Debug`           |
| `slogcp.LevelInfo`      | 0 (`slog.LevelInfo`)          | `INFO`              | `logging.Info`            |
| `slogcp.LevelNotice`    | 2                             | `NOTICE`            | `logging.Notice`          |
| `slogcp.LevelWarn`      | 4 (`slog.LevelWarn`)          | `WARN` (or `WARNING`) | `logging.Warning`         |
| `slogcp.LevelError`     | 8 (`slog.LevelError`)         | `ERROR`             | `logging.Error`           |
| `slogcp.LevelCritical`  | 12                            | `CRITICAL`          | `logging.Critical`        |
| `slogcp.LevelAlert`     | 16                            | `ALERT`             | `logging.Alert`           |
| `slogcp.LevelEmergency` | 20                            | `EMERGENCY`         | `logging.Emergency`       |

When logging to non-GCP targets (stdout, stderr, file), the `severity` field in the JSON output will use the GCP Severity String (e.g., "DEBUG", "NOTICE", "WARNING").

## HTTP Middleware Configuration (`slogcp/http`)

The `github.com/pjscruggs/slogcp/http` subpackage provides `net/http` middleware.

### `slogcphttp.Middleware`

`slogcphttp.Middleware(logger *slog.Logger) func(http.Handler) http.Handler`

Wraps an `http.Handler` to log request and response details.
-   **Input**: Requires an `*slog.Logger`. You can pass `slogcpLogger.Logger`.
-   **Log Content**:
    -   Method, URL, request size, status code, response size, latency, remote IP, user agent, referer, protocol.
    -   This information is structured into a `logging.HTTPRequest` object, which `slogcp`'s handler recognizes and places in the `httpRequest` field of the GCP log entry.
-   **Log Level**: Determined by response status:
    -   `5xx` -> `slog.LevelError`
    -   `4xx` -> `slog.LevelWarn`
    -   Others -> `slog.LevelInfo`
-   **Trace Context**: Extracts trace context from incoming headers using the globally configured OpenTelemetry propagator (e.g., W3C TraceContext, B3) and adds it to the request's `context.Context`. This context is then used for logging, enabling trace correlation.

### `slogcphttp.InjectTraceContextMiddleware`

`slogcphttp.InjectTraceContextMiddleware() func(http.Handler) http.Handler`

Specifically processes the `X-Cloud-Trace-Context` header (used by Google Cloud services like Load Balancers and App Engine) and injects the parsed trace information into the request's `context.Context`.
-   **Usage**: Place this middleware *before* `slogcphttp.Middleware` or any other middleware that consumes trace context if you need to ensure `X-Cloud-Trace-Context` is prioritized or handled when a global OTel propagator might not be configured for it.
    ```go
    handler := slogcphttp.Middleware(slogcpLogger.Logger)(
        slogcphttp.InjectTraceContextMiddleware()(myAppHandler),
    )
    ```
-   **Behavior**:
    -   If a valid trace context already exists in `r.Context()` (e.g., populated by another OTel middleware), this injector does nothing.
    -   If the `X-Cloud-Trace-Context` header is missing or invalid, it proceeds without modifying the context.
    -   If a valid header is found, it creates a `trace.SpanContext` and adds it to the request context using `trace.ContextWithRemoteSpanContext`.

## gRPC Interceptor Configuration (`slogcp/grpc`)

The `github.com/pjscruggs/slogcp/grpc` subpackage provides gRPC client and server interceptors.

### Common gRPC Options

These options are applicable to both client and server interceptors. They are passed to the interceptor constructor functions (e.g., `slogcpgrpc.UnaryServerInterceptor(logger, opts...)`).

-   **`WithLevels(f CodeToLevel)`**: Customizes the mapping from `google.golang.org/grpc/codes.Code` to `slog.Level` for the final log entry.
    -   `CodeToLevel` is `func(code codes.Code) slog.Level`.
    -   Default: Maps `OK` to `Info`, client errors (`InvalidArgument`, `NotFound`, etc.) to `Warn`, server errors (`Internal`, `Unknown`, etc.) to `Error`.
-   **`WithShouldLog(f ShouldLogFunc)`**: Filters which RPC calls are logged.
    -   `ShouldLogFunc` is `func(ctx context.Context, fullMethodName string) bool`.
    -   Return `false` to skip logging for a call. Useful for health checks.
    -   Default: Logs all calls. This is combined with `WithSkipPaths` and `WithSamplingRate`.
-   **`WithPayloadLogging(enabled bool)`**: Enables logging of request and response message payloads.
    -   Logged at `slog.LevelDebug`.
    -   Default: `false`. Use with caution due to potential log volume and data sensitivity.
-   **`WithMaxPayloadSize(sizeBytes int)`**: Limits the size of logged payloads (in bytes) when payload logging is enabled.
    -   Truncated payloads are marked, and original size is logged.
    -   Default: `0` (no limit).
-   **`WithMetadataLogging(enabled bool)`**: Enables logging of gRPC request/response metadata (headers/trailers).
    -   Default: `false`.
-   **`WithMetadataFilter(f MetadataFilterFunc)`**: Filters which metadata keys are logged.
    -   `MetadataFilterFunc` is `func(key string) bool`. Return `true` to include the key.
    -   Default: Excludes common sensitive headers like `authorization`, `cookie`, `grpc-trace-bin`.
-   **`WithPanicRecovery(enabled bool)`** (Server-side only): Controls if the interceptor recovers from panics in handlers, logs them, and returns a `codes.Internal` error.
    -   Default: `true`. Set to `false` if another interceptor handles panic recovery.
-   **`WithAutoStackTrace(enabled bool)`** (Server-side only): If `true`, automatically attaches stack traces to logged errors (from panics or error-level logs containing an `error` object).
    -   Default: `false`.
-   **`WithSkipPaths(paths []string)`**: Excludes specific gRPC methods from logging if their full method name contains any of the specified path strings.
    -   Example: `slogcpgrpc.WithSkipPaths([]string{"/grpc.health.v1.Health/Check"})`
-   **`WithSamplingRate(rate float64)`**: Logs a fraction of requests (0.0 to 1.0).
    -   Sampling is deterministic based on method name and timestamp.
    -   Default: `1.0` (log all requests).
-   **`WithLogCategory(category string)`**: Adds a `log.category` attribute to gRPC log entries.
    -   Default: `"grpc_request"`.

### Server Interceptors

-   `slogcpgrpc.UnaryServerInterceptor(logger *slogcp.Logger, opts ...Option)`
-   `slogcpgrpc.StreamServerInterceptor(logger *slogcp.Logger, opts ...Option)`

These interceptors log:
-   gRPC service and method names.
-   Duration of the call.
-   Final gRPC status code.
-   Peer address (client's address).
-   Error returned by the handler (formatted for Cloud Error Reporting).
-   Panic details if `WithPanicRecovery(true)` is active.
-   Trace context is automatically extracted from the incoming `context.Context` by the `slogcp.Logger`'s handler.

### Client Interceptors

-   `slogcpgrpc.NewUnaryClientInterceptor(logger *slogcp.Logger, opts ...Option)`
-   `slogcpgrpc.NewStreamClientInterceptor(logger *slogcp.Logger, opts ...Option)`

These interceptors log:
-   gRPC service and method names.
-   Duration of the call.
-   Final gRPC status code.
-   Error returned by the call.
-   Trace context is propagated in outgoing metadata and included in logs by the `slogcp.Logger`'s handler.

### Manual Trace Context Injection (gRPC)

-   `slogcpgrpc.InjectUnaryTraceContextInterceptor() grpc.UnaryServerInterceptor`
-   `slogcpgrpc.InjectStreamTraceContextInterceptor() grpc.StreamServerInterceptor`

These server interceptors read trace context headers (`traceparent` W3C header first, then `x-cloud-trace-context` GCP header) from incoming request metadata and inject the parsed `trace.SpanContext` into the Go `context.Context`.
-   **Usage**: Use only if standard OpenTelemetry instrumentation (e.g., `otelgrpc`) is NOT configured to handle trace propagation from these headers automatically.
-   Place early in the interceptor chain, before logging or other trace-consuming interceptors.
-   If a valid OTel span context already exists in the incoming context, these injectors do nothing.

## Environment Variables Summary

| Variable                             | Description                                                                 | Default (if any)                               |
|--------------------------------------|-----------------------------------------------------------------------------|------------------------------------------------|
| `LOG_LEVEL`                          | Minimum log level (debug, info, warn, error, notice, critical, etc.)        | `info`                                         |
| `LOG_SOURCE_LOCATION`                | Include source file/line in logs (`true`/`false`)                           | `false`                                        |
| `LOG_STACK_TRACE_ENABLED`            | Enable stack traces for errors (`true`/`false`)                             | `false`                                        |
| `LOG_STACK_TRACE_LEVEL`              | Minimum level for stack traces (e.g., `error`)                              | `error`                                        |
| `SLOGCP_LOG_TARGET`                  | Primary log destination (`gcp`, `stdout`, `stderr`, `file`)                 | `gcp`                                          |
| `SLOGCP_REDIRECT_AS_JSON_TARGET`     | Specific target for non-GCP modes (`stdout`, `stderr`, `file:/path/to.log`) | (depends on `SLOGCP_LOG_TARGET`)               |
| `SLOGCP_PROJECT_ID`                  | GCP Project ID (overrides `GOOGLE_CLOUD_PROJECT` and metadata)              | (derived)                                      |
| `GOOGLE_CLOUD_PROJECT`               | GCP Project ID (standard env var)                                           | (derived from metadata if on GCP)              |
| `SLOGCP_GCP_PARENT`                  | GCP Parent resource (e.g., `projects/ID`, `folders/ID`)                     | (derived from Project ID)                      |
| `SLOGCP_GCP_CLIENT_SCOPES`           | Comma-separated OAuth2 scopes for GCP client                                | (GCP client default)                           |
| `SLOGCP_GCP_COMMON_LABELS_JSON`      | JSON string map of common labels for GCP logs                               | (none)                                         |
| `SLOGCP_GCP_CL_*`                    | Individual common labels (e.g., `SLOGCP_GCP_CL_SERVICE=api`)                | (none)                                         |
| `SLOGCP_GCP_RESOURCE_TYPE`           | GCP Monitored Resource type (e.g., `gce_instance`)                          | (auto-detected)                                |
| `SLOGCP_GCP_RL_*`                    | GCP Monitored Resource labels (e.g., `SLOGCP_GCP_RL_INSTANCE_ID=123`)       | (auto-detected)                                |
| `SLOGCP_GCP_CONCURRENT_WRITE_LIMIT`  | Max concurrent goroutines for sending logs to GCP                           | 1                                              |
| `SLOGCP_GCP_DELAY_THRESHOLD_MS`      | Max time (ms) to buffer entries before sending to GCP                       | 1000 (1s)                                      |
| `SLOGCP_GCP_ENTRY_COUNT_THRESHOLD`   | Max number of entries to buffer before sending to GCP                       | 1000                                           |
| `SLOGCP_GCP_ENTRY_BYTE_THRESHOLD`    | Max size (bytes) of entries to buffer before sending to GCP                 | 8388608 (8MiB)                                 |
| `SLOGCP_GCP_ENTRY_BYTE_LIMIT`        | Max size (bytes) of a single log entry for GCP                              | 0 (no limit by slogcp)                         |
| `SLOGCP_GCP_BUFFERED_BYTE_LIMIT`     | Total memory limit (bytes) for buffered log entries for GCP                 | 104857600 (100MiB)                             |
| `SLOGCP_GCP_CONTEXT_FUNC_TIMEOUT_MS` | Timeout (ms) for default context used in background GCP client operations   | (none, client init has 10s default)            |
| `SLOGCP_GCP_PARTIAL_SUCCESS`         | Enable partial success for GCP log writes (`true`/`false`)                  | `false`                                        |

## Examples

### Basic GCP Logging

This setup relies on environment variables like `GOOGLE_CLOUD_PROJECT` or running on GCP for project detection.

```go
package main

import (
	"log"
	"time"

	"github.com/pjscruggs/slogcp"
)

func main() {
	logger, err := slogcp.New()
	if err != nil {
		log.Fatalf("Failed to create logger: %v", err)
	}
	defer logger.Close() // Crucial for flushing logs

	logger.Info("Application started", "version", "1.0.0", "pid", 12345)
	logger.Error("Something went wrong", "error", "simulated error", "retry_count", 3)
	
    // Using extended levels
    logger.NoticeContext(context.Background(), "User signed up", "user_id", "xyz123")
    logger.CriticalContext(context.Background(), "Database connection lost", "db_host", "prod-db-1")
}
```

### Local Development to stdout with Debug Level

Configure the logger to output structured JSON to `stdout` with debug level and source location.

```go
package main

import (
	"log"
	"log/slog" // For slog.LevelDebug

	"github.com/pjscruggs/slogcp"
)

func main() {
	logger, err := slogcp.New(
		slogcp.WithRedirectToStdout(),
		slogcp.WithLevel(slog.LevelDebug), // Use standard slog.LevelDebug
		slogcp.WithSourceLocationEnabled(true),
	)
	if err != nil {
		log.Fatalf("Failed to create logger: %v", err)
	}
	defer logger.Close()

	logger.Debug("This is a debug message", "detail", "some internal state")
	logger.Info("Application running in local mode")

    if logger.IsInFallbackMode() {
        logger.Info("Logger is in automatic fallback mode (e.g. GCP init failed, now logging to stdout).")
    }
}
```
Alternatively, set environment variables:
`SLOGCP_LOG_TARGET=stdout LOG_LEVEL=debug LOG_SOURCE_LOCATION=true`

### Logging to a File with Rotation (using Lumberjack)

Integrate with `gopkg.in/natefinch/lumberjack.v2` for file logging with rotation.

```go
package main

import (
	"log"

	"github.com/pjscruggs/slogcp"
	"gopkg.in/natefinch/lumberjack.v2"
)

func main() {
	logFile := &lumberjack.Logger{
		Filename:   "/var/log/myapp/app.log",
		MaxSize:    100, // megabytes
		MaxBackups: 3,
		MaxAge:     28, // days
		Compress:   true,
	}

	logger, err := slogcp.New(
		slogcp.WithRedirectWriter(logFile), // Pass the lumberjack writer
		slogcp.WithLevel(slogcp.LevelInfo),
	)
	if err != nil {
		log.Fatalf("Failed to create logger: %v", err)
	}
	// logger.Close() will also call logFile.Close() because lumberjack.Logger implements io.Closer
	defer logger.Close() 

	logger.Info("Logging to a rotating file", "path", logFile.Filename)
}
```

### Customizing gRPC Server Logging

Configure gRPC server interceptors to skip health checks and log payloads for debugging.

```go
package main

import (
	"context"
	"log"
	"net"
	"strings"
	"time"

	"github.com/pjscruggs/slogcp"
	slogcpgrpc "github.com/pjscruggs/slogcp/grpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health/grpc_health_v1" // For health check example
	// ... your_pb_definitions
)

// Example HealthServer implementation
type healthServer struct{}
func (s *healthServer) Check(ctx context.Context, req *grpc_health_v1.HealthCheckRequest) (*grpc_health_v1.HealthCheckResponse, error) {
	return &grpc_health_v1.HealthCheckResponse{Status: grpc_health_v1.HealthCheckResponse_SERVING}, nil
}
func (s *healthServer) Watch(req *grpc_health_v1.HealthCheckRequest, stream grpc_health_v1.Health_WatchServer) error {
	return status.Errorf(codes.Unimplemented, "Watch is not implemented")
}


func main() {
	slogcpLogger, err := slogcp.New(slogcp.WithRedirectToStdout()) // Local example
	if err != nil {
		log.Fatalf("Failed to create slogcp logger: %v", err)
	}
	defer slogcpLogger.Close()

	// Custom function to skip logging for health checks
	shouldLogRPC := func(ctx context.Context, fullMethodName string) bool {
		if strings.HasPrefix(fullMethodName, "/grpc.health.v1.Health/") {
			return false // Don't log health checks
		}
		return true
	}

	// Custom level function (example: treat NotFound as Info)
	customCodeToLevel := func(code codes.Code) slog.Level {
		if code == codes.NotFound {
			return slog.LevelInfo
		}
		return slogcpgrpc.DefaultCodeToLevel()(code) // Fallback to default for others
	}

	server := grpc.NewServer(
		grpc.ChainUnaryInterceptor(
			slogcpgrpc.UnaryServerInterceptor(slogcpLogger,
				slogcpgrpc.WithShouldLog(shouldLogRPC),
				slogcpgrpc.WithLevels(customCodeToLevel),
				slogcpgrpc.WithPayloadLogging(true),      // Enable payload logging
				slogcpgrpc.WithMaxPayloadSize(1024),     // Limit payload log to 1KB
				slogcpgrpc.WithMetadataLogging(true),    // Enable metadata logging
			),
			// ... other unary interceptors
		),
		grpc.ChainStreamInterceptor(
			slogcpgrpc.StreamServerInterceptor(slogcpLogger,
				slogcpgrpc.WithShouldLog(shouldLogRPC),
				// ... other stream options
			),
			// ... other stream interceptors
		),
	)

	// Register your services
	// pb.RegisterYourServiceServer(server, &yourServiceImpl{})
	grpc_health_v1.RegisterHealthServer(server, &healthServer{})


	lis, err := net.Listen("tcp", ":50051")
	if err != nil {
		log.Fatalf("Failed to listen: %v", err)
	}

	slogcpLogger.Info("gRPC server starting", "address", lis.Addr().String())
	if err := server.Serve(lis); err != nil {
		slogcpLogger.Error("gRPC server failed", "error", err)
	}
}
```
