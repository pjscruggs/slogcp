# slogcp Configuration

This document provides a comprehensive guide to configuring the `slogcp` library and its subpackages for HTTP and gRPC logging.

## Overview

`slogcp` uses a layered configuration approach:
1.  **Defaults**: Sensible defaults are applied first.
2.  **Environment Variables**: Override defaults. These are loaded when `slogcp.NewHandler(defaultWriter)` is called.
3.  **Programmatic Options**: Options passed to `slogcp.NewHandler(defaultWriter, opts ...Option)` override both defaults and environment variables.

This allows for flexible configuration suitable for various deployment environments.

### Boolean Environment Variables

All boolean environment variables in `slogcp` accept `true`, `1`, `yes`, or `on` (case-insensitive) to enable a feature. They accept `false`, `0`, `no`, or `off` to disable it. If any other value is supplied, the configuration keeps its default.

## Core Handler Configuration (`slogcp.NewHandler`)

These options configure the main `*slog.Logger` instance.

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
-   **Default**: `stdout` (`slogcp.LogTargetStdout`). See [Automatic Fallback Mode](#automatic-fallback-mode) for behavior when `LogTargetGCP` is used.

**Target-Specific Notes:**

#### GCP Cloud Logging API
-   Requires GCP project information (see [GCP Project ID and Parent Resource](#gcp-project-id-and-parent-resource)).
-   Uses the `cloud.google.com/go/logging` client.
-   Log entries are sent to a log named `app` by default (configurable, see [Log ID](#log-id)).

#### Standard Output (stdout) (Default)
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
-   If the provided writer implements `io.Closer`, `handler.Close()` will call the writer's `Close()` method.

### Source Code Location

Includes the source file, line number, and function name in log entries.

-   **Programmatic**: `slogcp.WithSourceLocationEnabled(enabled bool)`
-   **Environment Variable**: `LOG_SOURCE_LOCATION`
    -   Example: `LOG_SOURCE_LOCATION=true`
-   **Default**: `false` (disabled)
-   Enabling this adds some performance overhead.

### Stack Traces

Automatically captures stack traces for logs at or above a specified level, typically for errors.

-   **Programmatic**:
    -   `slogcp.WithStackTraceEnabled(enabled bool)`
    -   `slogcp.WithStackTraceLevel(level slog.Level)`
-   **Environment Variables**:
    -   `LOG_STACK_TRACE_ENABLED`
        -   Example: `LOG_STACK_TRACE_ENABLED=true`
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

**Note**: These settings are used to configure the GCP API client. If the `LogTarget` is not `LogTargetGCP`, these settings have no effect on where logs are sent, but the `ProjectID` is still used to format the `trace` field in structured JSON logs.

Required when `LogTarget` is `LogTargetGCP`. The Parent resource (e.g., `projects/PROJECT_ID`) determines where logs are written. The Project ID is also used for formatting trace IDs.

-   **Programmatic**:
    -   `slogcp.WithProjectID(id string)`: Sets the Project ID. Infers Parent as `projects/ID`.
    -   `slogcp.WithParent(parent string)`: Sets the Parent (e.g., `projects/ID`, `folders/ID`, `organizations/ID`, `billingAccounts/ID`). If project-based, infers Project ID.
-   **Environment Variables (in order of precedence for Project ID/Parent derivation)**:
    1.  `SLOGCP_GCP_PARENT`: Explicitly sets the parent. If project-based (e.g., `projects/my-gcp-project`), `my-gcp-project` is used as Project ID.
    2.  `SLOGCP_PROJECT_ID`: Sets the Project ID. Parent defaults to `projects/YOUR_SLOGCP_PROJECT_ID`.
    3.  `GOOGLE_CLOUD_PROJECT`: Standard GCP environment variable for Project ID. Parent defaults to `projects/YOUR_GOOGLE_CLOUD_PROJECT`.
-   **Metadata Server**: If running on GCP (e.g., GCE, GKE, Cloud Run) and no environment variables are set, the Project ID is auto-detected from the metadata server. Parent defaults to `projects/DETECTED_PROJECT_ID`.
-   **Default**: None. If `LogTargetGCP` is used and no Project ID/Parent can be resolved, `slogcp.NewHandler(defaultWriter)` will either return an error or automatically fall back to `stdout` logging (see below).

### Cross-project Trace Linking

**Note**: This setting is used to format the `trace` field in structured logs, regardless of the `LogTarget`. It is most relevant when logs from one project need to link to traces in another.

When logs are written in one project but traces live in another, you can direct trace formatting to a different project.

-   **Programmatic**: `slogcp.WithTraceProjectID(id string)`
-   **Environment Variable**: `SLOGCP_TRACE_PROJECT_ID`
-   **Behavior**: The handler formats the `Trace` as `projects/{TraceProjectID}/traces/{traceID}` while still writing log entries under the configured parent. If `TraceProjectID` is not set, it falls back to the main `ProjectID`.

### Automatic Fallback Mode

If `LogTargetGCP` is explicitly configured (e.g., via `slogcp.WithLogTarget(slogcp.LogTargetGCP)` or `SLOGCP_LOG_TARGET=gcp`) and initialization fails (e.g., due to missing credentials or project ID when running locally), `slogcp.NewHandler(defaultWriter)` will automatically fall back to structured JSON logging on `stdout`.

This behavior is designed to simplify local development, allowing the same code to log to GCP in production and `stdout` locally without changes.

-   **Detection**: Use `logger.IsInFallbackMode() bool` to check if the logger is operating in this fallback mode.
-   **Suppression**: Fallback is suppressed if any explicit redirect target is configured. This includes using options like `WithRedirectToFile()`, `WithRedirectToStdout()`, `WithRedirectToStderr()`, `WithRedirectWriter()`, or setting the `SLOGCP_REDIRECT_AS_JSON_TARGET` environment variable. In these cases, initialization errors are returned directly.

## HTTP Middleware Options (`slogcp/http`)

Unless configured otherwise, the middleware logs every request, does not recover from panics, captures no headers or bodies, and resolves the client IP from the remote socket address. Server errors (HTTP 5xx) always generate log entries, even when trace-based suppression is enabled.

### Functional Options

| Option | Description |
| --- | --- |
| `WithShouldLog(func(context.Context, *http.Request) bool)` | Predicate invoked after trace extraction to decide whether a request should emit a log entry. |
| `WithSkipPathSubstrings(substrings ...string)` | Drops requests whose `URL.Path` contains any of the supplied substrings. |
| `WithSuppressUnsampledBelow(level slog.Leveler)` | Suppresses logs for unsampled traces below the provided severity. Server errors (5xx) are never suppressed. |
| `WithLogRequestHeaderKeys(keys ...string)` / `WithLogResponseHeaderKeys(keys ...string)` | Records selected headers into structured attributes. Keys are canonicalised like `net/http`. |
| `WithRequestBodyLimit(limit int64)` / `WithResponseBodyLimit(limit int64)` | Captures up to `limit` bytes of the body for debugging. Zero disables capture. |
| `WithRecoverPanics(enabled bool)` | Wraps the handler with panic recovery that logs and converts panics into HTTP 500 responses. |
| `WithTrustProxyHeaders(enabled bool)` | When true, `X-Forwarded-For` and `X-Real-IP` headers are trusted when computing the client IP. |
| `WithHealthCheckFilter(cfg healthcheck.Config)` | Enables configurable health-check detection (disabled by default). Supports tagging, demoting, or dropping matched probes without imposing specific paths or headers. |

The shared filter type lives in `github.com/pjscruggs/slogcp/healthcheck`. It ships with disabled defaults so you can opt in explicitly and refine the behaviour you want:

```go
import (
    slogcphttp "github.com/pjscruggs/slogcp/http"
    "github.com/pjscruggs/slogcp/healthcheck"
)

hc := healthcheck.DefaultConfig()
hc.Enabled = true
hc.Mode = healthcheck.ModeDemote
hc.Paths = []string{"/healthz"}

middleware := slogcphttp.Middleware(logger, slogcphttp.WithHealthCheckFilter(hc))
```

### Environment Variables

| Variable | Purpose | Default |
| --- | --- | --- |
| `SLOGCP_HTTP_SKIP_PATH_SUBSTRINGS` | Comma-separated list mirroring `WithSkipPathSubstrings`. | (none) |
| `SLOGCP_HTTP_SUPPRESS_UNSAMPLED_BELOW` | Severity threshold string or integer for `WithSuppressUnsampledBelow` (e.g. `WARNING`, `DEFAULT`). | (none) |
| `SLOGCP_HTTP_LOG_REQUEST_HEADER_KEYS` | Comma-separated request header keys to capture. | (none) |
| `SLOGCP_HTTP_LOG_RESPONSE_HEADER_KEYS` | Comma-separated response header keys to capture. | (none) |
| `SLOGCP_HTTP_REQUEST_BODY_LIMIT` | Integer byte limit for request body capture. | `0` (disabled) |
| `SLOGCP_HTTP_RESPONSE_BODY_LIMIT` | Integer byte limit for response body capture. | `0` (disabled) |
| `SLOGCP_HTTP_RECOVER_PANICS` | Enables panic recovery when set to true. | `false` |
| `SLOGCP_HTTP_TRUST_PROXY_HEADERS` | Enables proxy header trust when set to true. | `false` |
| `SLOGCP_HTTP_SKIP_GOOGLE_HEALTHCHECKS` | Legacy toggle that enables the health-check filter in drop mode when set to true. | `false` |

Invalid values are ignored so that programmatic options can supply explicit overrides without extra error handling.

### Closing the Logger

It's crucial to close the logger to ensure all buffered logs are flushed and resources are released.

-   **Method**: `handler.Close() error`
-   Call this method during application shutdown, typically using `defer handler.Close()`.
-   `Close()` is idempotent (safe to call multiple times).
-   Behavior:
    -   **GCP Mode**: Flushes logs to Cloud Logging and closes the client.
    -   **File Mode (slogcp-managed)**: Closes the log file opened by `slogcp`.
    -   **Custom `io.Writer` Mode**: If the writer implements `io.Closer`, its `Close()` method is called.

## GCP Client Options (for `LogTargetGCP`)

**Note**: The options in this section configure the GCP Cloud Logging API client. They have no effect if the `LogTarget` is not `LogTargetGCP` (e.g., when logging to `stdout`, `stderr`, or a file).

These options fine-tune the behavior of the underlying Google Cloud Logging client when `LogTarget` is `LogTargetGCP`. Many correspond to options in `cloud.google.com/go/logging.ClientOptions` or `logging.LoggerOptions`.

### Log ID

Identifies the specific log within Google Cloud Logging where entries are written. In the Cloud Logging entry structure, it appears as the final component of the `logName` field: `projects/{PROJECT_ID}/logs/{LOG_ID}`.

-   **Programmatic**: `slogcp.WithGCPLogID(logID string)`
    -   Example: `slogcp.WithGCPLogID("my-service")`
-   **Environment Variable**: `SLOGCP_GCP_LOG_ID`
-   **Default**: `"app"`
-   **Validation**: Must be less than 512 characters and contain only letters, digits, and the characters `/`, `_`, `-`, `.`. Leading/trailing whitespace and surrounding quotes are automatically removed.

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
-   **`WithGCPEntryByteLimit(n int)` / `SLOGCP_GCP_ENTRY_BYTE_LIMIT`****:**
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
| `slogcp.LevelWarn`      | 4 (`slog.LevelWarn`)          | `WARN` (or `WARNING`) | `logging.Warning`       |
| `slogcp.LevelError`     | 8 (`slog.LevelError`)         | `ERROR`             | `logging.Error`           |
| `slogcp.LevelCritical`  | 12                            | `CRITICAL`          | `logging.Critical`        |
| `slogcp.LevelAlert`     | 16                            | `ALERT`             | `logging.Alert`           |
| `slogcp.LevelEmergency` | 20                            | `EMERGENCY`         | `logging.Emergency`       |

When logging to non-GCP targets (stdout, stderr, file), the `severity` field in the JSON output will use the GCP Severity String (e.g., "DEBUG", "NOTICE", "WARNING").

## HTTP Middleware & Client Propagation (`slogcp/http`)

The `github.com/pjscruggs/slogcp/http` subpackage provides server middleware **and** an optional client transport.

### `slogcphttp.Middleware`

`slogcphttp.Middleware(logger *slog.Logger) func(http.Handler) http.Handler`

Wraps an `http.Handler` to log request and response details.
-   **Input**: Requires an `*slog.Logger`. You can pass `logger`.
-   **Log Content**:
    -   Method, URL, request size (from `Content-Length`), status code, response size, latency, and remote IP.
    -   These fields populate a `logging.HTTPRequest`, which `slogcp`'s handler forwards to the `httpRequest` field in Cloud Logging.
    -   Optional request/response headers and body excerpts can be captured through the functional options described below.
-   **Log Level**: Determined by response status:
    -   `5xx` -> `slog.LevelError`
    -   `4xx` -> `slog.LevelWarn`
    -   Others -> `slog.LevelInfo`
-   **Trace Context (inbound)**: Extracts trace context from incoming headers using the globally configured OpenTelemetry propagator (e.g., W3C TraceContext, B3) and adds it to the request's `context.Context`. This context is then used for logging, enabling trace correlation.

### `slogcphttp.InjectTraceContextMiddleware`

`slogcphttp.InjectTraceContextMiddleware() func(http.Handler) http.Handler`

Specifically processes the `X-Cloud-Trace-Context` header (used by Google Cloud services like Load Balancers and App Engine) and injects the parsed trace information into the request's `context.Context`.
-   **Usage**: Place this middleware *before* `slogcphttp.Middleware` or any other middleware that consumes trace context if you need to ensure `X-Cloud-Trace-Context` is prioritized or handled when a global OTel propagator might not be configured for it.
    ```go
    handler := slogcphttp.Middleware(logger)(
        slogcphttp.InjectTraceContextMiddleware()(myAppHandler),
    )
    ```
-   **Behavior**:
    -   If a valid trace context already exists in `r.Context()` (e.g., populated by another OTel middleware), this injector does nothing.
    -   If the `X-Cloud-Trace-Context` header is missing or invalid, it proceeds without modifying the context.
    -   If a valid header is found, it creates a `trace.SpanContext` and adds it to the request context using `trace.ContextWithRemoteSpanContext`.

### `slogcphttp.NewTraceRoundTripper`

`NewTraceRoundTripper(base http.RoundTripper, opts ...TraceRoundTripperOption) http.RoundTripper`

An opt-in HTTP client transport that **propagates the current trace context on outbound requests**:
-   Injects **W3C Trace Context** (`traceparent`/`tracestate`) by default.
-   Injects **`X-Cloud-Trace-Context`** (span ID in **decimal**; `o=1` when sampled) unless disabled.
-   Skips injection if headers are already present on the request.
-   You can restrict propagation using a predicate.

**Options**
- `WithInjectTraceparent(enabled bool)` — defaults to `true`.
- `WithInjectXCloud(enabled bool)` — defaults to `true`.
- `WithSkip(func(*http.Request) bool)` — return `true` to skip propagation (e.g., for external domains).

**Usage**
```go
client := &http.Client{
    Transport: slogcphttp.NewTraceRoundTripper(nil, // wrap DefaultTransport
        slogcphttp.WithSkip(func(r *http.Request) bool {
            // Skip propagation when the host is outside the internal suffix list.
            return !strings.HasSuffix(r.URL.Host, ".svc.cluster.local") &&
                   !strings.HasSuffix(r.URL.Host, ".corp.example.com")
        }),
    ),
}

req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, "http://svc.api/", nil)
resp, err := client.Do(req)
```

> If you already use `otelhttp.NewTransport`, prefer that for full tracing (it injects W3C). Do not double-wrap. The transport here is for lightweight **propagation** (log/trace correlation) without full OTel.

## gRPC Interceptor Configuration (`slogcp/grpc`)

The `github.com/pjscruggs/slogcp/grpc` subpackage provides gRPC client and server interceptors.

### Common gRPC Options

These options are applicable to both client and server interceptors. They are passed to the interceptor constructor functions (e.g., `slogcpgrpc.UnaryServerInterceptor(logger, opts...)`).

* **`WithLevels(f CodeToLevel)`**: Customizes the mapping from `google.golang.org/grpc/codes.Code` to `slog.Level` for the final log entry.
* **`WithShouldLog(f ShouldLogFunc)`**: Filters which RPC calls are logged.
* **`WithPayloadLogging(enabled bool)`**: Enables logging of request and response message payloads (debug level).
* **`WithMaxPayloadSize(sizeBytes int)`**: Limits the size of logged payloads (when enabled).
* **`WithMetadataLogging(enabled bool)`**: Enables logging of gRPC metadata (headers/trailers).
* **`WithMetadataFilter(f MetadataFilterFunc)`**: Filters which metadata keys are logged.
* **`WithPanicRecovery(enabled bool)`** (Server): Interceptor recovers and logs panics as `Internal`.
* **`WithAutoStackTrace(enabled bool)`** (Server): Attach stack traces to logged errors.
* **`WithSkipPaths(paths []string)`**: Exclude method paths from logging.
* **`WithHealthCheckFilter(cfg healthcheck.Config)`**: Shares the same configurable health-check detection used by the HTTP middleware so you can tag, demote, or drop common probes without hand-written filters.
* **`WithSamplingRate(rate float64)`**: Sample logs (0.0..1.0).
* **`WithLogCategory(category string)`**: Adds a `log.category` attribute (default `"grpc_request"`).
* **`WithTracePropagation(enabled bool)`**: When `true` (default), **client** interceptors inject **W3C** (`traceparent`/`tracestate`) into outgoing metadata using the global OTel propagator. They **do not** add `x-cloud-trace-context` to gRPC metadata. If `traceparent` already exists, they do not overwrite it.

The filter example shown for HTTP also applies here; reuse the same `healthcheck.Config` to keep behaviour consistent across transport layers.

### Server Interceptors

* `slogcpgrpc.UnaryServerInterceptor(logger *slog.Logger, opts ...Option)`
* `slogcpgrpc.StreamServerInterceptor(logger *slog.Logger, opts ...Option)`

These interceptors log:

* gRPC service and method names.
* Duration of the call.
* Final gRPC status code.
* Peer address (client's address).
* Error returned by the handler (formatted for Cloud Error Reporting).
* Panic details if `WithPanicRecovery(true)` is active.

Trace context is extracted from the incoming `context.Context` and included in logs by the `slogcp` handler.

### Client Interceptors

* `slogcpgrpc.NewUnaryClientInterceptor(logger *slog.Logger, opts ...Option)`
* `slogcpgrpc.NewStreamClientInterceptor(logger *slog.Logger, opts ...Option)`

These interceptors log:

* gRPC service and method names.
* Duration of the call.
* Final gRPC status code.
* Error returned by the call.

When `WithTracePropagation(true)` (default), they inject W3C trace context into outgoing metadata using the global OTel propagator and **skip injection** if a `traceparent` is already present.

### Manual Trace Context Injection (gRPC)

* `slogcpgrpc.InjectUnaryTraceContextInterceptor() grpc.UnaryServerInterceptor`
* `slogcpgrpc.InjectStreamTraceContextInterceptor() grpc.StreamServerInterceptor`

These server interceptors read trace context headers (`traceparent` W3C header first, then `x-cloud-trace-context` GCP header) from incoming request metadata and inject the parsed `trace.SpanContext` into the Go `context.Context`.

* **Usage**: Use only if standard OpenTelemetry instrumentation (e.g., `otelgrpc`) is NOT configured to handle trace propagation from these headers automatically.
* Place early in the interceptor chain, before logging or other trace-consuming interceptors.
* If a valid OTel span context already exists in the incoming context, these injectors do nothing.

## Operation Grouping

To group related log entries in Cloud Logging, emit the standard operation structure:

```go
logger.Info("request start",
    slog.Group("logging.googleapis.com/operation",
        slog.String("id", requestID),
        slog.String("producer", "api-server"),
        slog.Bool("first", true),
    ),
)
```

`slogcp` will map this group to the Cloud Logging `operation` field when using the API client, and emit the same nested object in JSON redirect modes.

## Environment Variables Summary

| Variable                             | Description                                                                 | Default (if any)                    |
| ------------------------------------ | --------------------------------------------------------------------------- | ----------------------------------- |
| `LOG_LEVEL`                          | Minimum log level (debug, info, warn, error, notice, critical, etc.)        | `info`                              |
| `LOG_SOURCE_LOCATION`                | Include source file/line in logs (`true`/`false`)                           | `false`                             |
| `LOG_STACK_TRACE_ENABLED`            | Enable stack traces for errors (`true`/`false`)                             | `false`                             |
| `LOG_STACK_TRACE_LEVEL`              | Minimum level for stack traces (e.g., `error`)                              | `error`                             |
| `SLOGCP_LOG_TARGET`                  | Primary log destination (`gcp`, `stdout`, `stderr`, `file`)                 | `stdout`                            |
| `SLOGCP_REDIRECT_AS_JSON_TARGET`     | Specific target for non-GCP modes (`stdout`, `stderr`, `file:/path/to.log`) | (none)                              |
| `SLOGCP_PROJECT_ID`                  | GCP Project ID (overrides `GOOGLE_CLOUD_PROJECT` and metadata)              | (derived)                           |
| `GOOGLE_CLOUD_PROJECT`               | GCP Project ID (standard env var)                                           | (derived from metadata if on GCP)   |
| `SLOGCP_GCP_PARENT`                  | GCP Parent resource (e.g., `projects/ID`, `folders/ID`)                     | (derived from Project ID)           |
| `SLOGCP_GCP_LOG_ID`                  | GCP Log ID (the name of the log within Cloud Logging)                       | `app`                               |
| `SLOGCP_GCP_CLIENT_SCOPES`           | Comma-separated OAuth2 scopes for GCP client                                | (GCP client default)                |
| `SLOGCP_GCP_COMMON_LABELS_JSON`      | JSON string map of common labels for GCP logs                               | (none)                              |
| `SLOGCP_GCP_CL_*`                    | Individual common labels (e.g., `SLOGCP_GCP_CL_SERVICE=api`)                | (none)                              |
| `SLOGCP_GCP_RESOURCE_TYPE`           | GCP Monitored Resource type (e.g., `gce_instance`)                          | (auto-detected)                     |
| `SLOGCP_GCP_RL_*`                    | GCP Monitored Resource labels (e.g., `SLOGCP_GCP_RL_INSTANCE_ID=123`)       | (auto-detected)                     |
| `SLOGCP_GCP_CONCURRENT_WRITE_LIMIT`  | Max concurrent goroutines for sending logs to GCP                           | 1                                   |
| `SLOGCP_GCP_DELAY_THRESHOLD_MS`      | Max time (ms) to buffer entries before sending to GCP                       | 1000 (1s)                           |
| `SLOGCP_GCP_ENTRY_COUNT_THRESHOLD`   | Max number of entries to buffer before sending to GCP                       | 1000                                |
| `SLOGCP_GCP_ENTRY_BYTE_THRESHOLD`    | Max size (bytes) of entries to buffer before sending to GCP                 | 8388608 (8MiB)                      |
| `SLOGCP_GCP_ENTRY_BYTE_LIMIT`        | Max size (bytes) of a single log entry for GCP                              | 0 (no limit by slogcp)              |
| `SLOGCP_GCP_BUFFERED_BYTE_LIMIT`     | Total memory limit (bytes) for buffered log entries for GCP                 | 104857600 (100MiB)                  |
| `SLOGCP_GCP_CONTEXT_FUNC_TIMEOUT_MS` | Timeout (ms) for default context used in background GCP client operations   | (none, client init has 10s default) |
| `SLOGCP_GCP_PARTIAL_SUCCESS`         | Enable partial success for GCP log writes (`true`/`false`)                  | `false`                             |
| `SLOGCP_TRACE_PROJECT_ID`            | Project ID used to format fully-qualified trace names                       | (falls back to main Project ID)     |

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
	logger, err := slogcp.NewHandler(defaultWriter)
	if err != nil {
		log.Fatalf("Failed to create logger: %v", err)
	}
	defer handler.Close() // Crucial for flushing logs

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
	handler, err := slogcp.NewHandler(os.Stdout,

		slogcp.WithRedirectToStdout(),
		slogcp.WithLevel(slog.LevelDebug), // Use standard slog.LevelDebug
		slogcp.WithSourceLocationEnabled(true),
	)
	if err != nil {
		log.Fatalf("Failed to create logger: %v", err)
	}
	defer handler.Close()

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

	handler, err := slogcp.NewHandler(os.Stdout,

		slogcp.WithRedirectWriter(logFile), // Pass the lumberjack writer
		slogcp.WithLevel(slogcp.LevelInfo),
	)
	if err != nil {
		log.Fatalf("Failed to create logger: %v", err)
	}
	// handler.Close() will also call logFile.Close() because lumberjack.Logger implements io.Closer
	defer handler.Close() 

	logger.Info("Logging to a rotating file", "path", logFile.Filename)
}
```

### Customizing gRPC Server Logging

Configure gRPC server interceptors to demote health checks and log payloads for debugging.

```go
package main

import (
	"log"
	"log/slog"
	"net"
	"os"

	"github.com/pjscruggs/slogcp"
	slogcpgrpc "github.com/pjscruggs/slogcp/grpc"
	"github.com/pjscruggs/slogcp/healthcheck"
	"google.golang.org/grpc"
	// ... your_pb_definitions
)

func main() {
	handler, err := slogcp.NewHandler(os.Stdout,
		slogcp.WithRedirectToStdout(),
	) // Local example
	if err != nil {
		log.Fatalf("Failed to create slogcp logger: %v", err)
	}
	defer handler.Close()
	logger := slog.New(handler)

	hc := healthcheck.DefaultConfig()
	hc.Enabled = true
	hc.Mode = healthcheck.ModeDemote
	hc.Methods = []string{"/grpc.health.v1.Health/Check", "/grpc.health.v1.Health/Watch"}

	server := grpc.NewServer(
		grpc.ChainUnaryInterceptor(
			slogcpgrpc.UnaryServerInterceptor(logger,
				slogcpgrpc.WithHealthCheckFilter(hc),
				slogcpgrpc.WithMetadataLogging(true),
			),
		),
		grpc.ChainStreamInterceptor(
			slogcpgrpc.StreamServerInterceptor(logger,
				slogcpgrpc.WithHealthCheckFilter(hc),
			),
		),
	)

	// Register your services...
	// pb.RegisterYourServiceServer(server, &yourServiceImpl{})

	lis, err := net.Listen("tcp", ":50051")
	if err != nil {
		log.Fatalf("Failed to listen: %v", err)
	}

	logger.Info("gRPC server starting", "address", lis.Addr().String())
	if err := server.Serve(lis); err != nil {
		logger.Error("gRPC server failed", "error", err)
	}
}
```

### HTTP Client Propagation with `PropagatingTransport`

Propagate the current trace to downstream HTTP services (for log/trace correlation) without pulling in full tracing:

```go
client := &http.Client{
    Transport: slogcphttp.NewTraceRoundTripper(nil, // wrap DefaultTransport
        slogcphttp.WithSkip(func(r *http.Request) bool {
            return !strings.HasSuffix(r.URL.Host, ".svc.cluster.local")
        }),
        // slogcphttp.WithInjectXCloud(false), // opt out of legacy header if desired
    ),
}

// Carry the inbound context so headers can be injected
req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, "http://orders.svc.cluster.local/create", body)
resp, err := client.Do(req)
```

> Downstream services that use `slogcp` (or the Cloud Logging Go client) will auto-link their logs to the same trace when `HTTPRequest.Request` is passed in their handler and the headers are present.



