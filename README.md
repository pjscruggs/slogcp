# slogcp

<img src="logo.svg" width="50%" alt="slogcp logo">

A "batteries included" structured logging module for Google Cloud Platform with built-in HTTP and gRPC interceptors.

## Installation

```bash
go get github.com/pjscruggs/slogcp
```

## Features

- {🪵}  **Structured JSON logging** for powerful filtering and analysis in Cloud Logging
- ☁️ **Cloud Logging-compatible JSON formatting** automatically shapes every entry for Google Cloud ingestion
- 🎚️ **Complete GCP severity level support** (DEBUG, INFO, NOTICE, WARNING, ERROR, CRITICAL, ALERT, EMERGENCY)
- 🔗 **Automatic trace context extraction and propagation** (gRPC interceptors and HTTP middleware/transport) that read the current OpenTelemetry span from `context.Context` and emit Cloud Logging-compatible trace/span IDs
- 🚚 **Optional HTTP client transport** that injects W3C Trace Context (and optionally `X-Cloud-Trace-Context`) on outbound requests
- 🧩 **Ready-to-use HTTP and gRPC middleware** with optimized GCP-friendly log structuring
- 🎛️ **Dynamic log level control** without application restart
- 🐛 **Error logging with optional stack traces** for efficient debugging
- 📡 **Automatic GCP resource detection** for proper log association

## Why Would I Use This?

### Who is this for?

You're a Go developer using:

- **these Google Cloud services**
  - Cloud Run Services
  - Cloud Run Jobs
  - Cloud Functions
  - App Engine
  - Google Kubernetes Engine

- **Google Cloud's native observability stack**  
  You rely on [Cloud Logging](https://docs.cloud.google.com/logging/docs), [Cloud Trace](https://docs.cloud.google.com/trace/docs), [Error Reporting](https://docs.cloud.google.com/error-reporting/docs), and related tools to understand your services, and you want logs, traces, and errors to line up cleanly in those UIs with correct severities, structured fields, trace IDs, and stack traces--without wiring all of that by hand in every service.

### What Problems Does This Solve?

#### `log/slog` Isn't Compatible With GCP Observability By Default
`log/slog` intentionally stays vendor-neutral: it emits `level` instead of `severity`, never sets `logging.googleapis.com/trace`, and doesn't populate `httpRequest` or `serviceContext` for Error Reporting. You can bolt on custom replacers and middleware to get there, but you end up rebuilding the same Cloud Logging fields in every service. `slogcp` bakes in the GCP-specific wiring (Cloud Logging severity names, trace/span correlation, Error Reporting service context, and optional request payloads) so your logs show up correctly in Logs Explorer, Trace, and Error Reporting without bespoke adapters.

#### Using `cloud.google.com/go/logging` is more expensive than logging to `stdout`

> [!NOTE]
> The official GCP documentation for the various services that support automatic ingestion of logs written to stdout/stderr doesn't have a consistent term for this feature. Lacking an official term, we'll be referring to this service as the "**logging ingester**."

CPU time is money. When you use a Cloud Logging client library and let it send logs to the Cloud Logging API, **your** billable service is responsible for marshaling every record into protobuf, maintaining gRPC streams, retrying transient failures, and batching writes across worker goroutines. If you don't configure the client correctly, [this can kill your performance](https://dev.to/siddhantkcode/2x-faster-40-less-ram-the-cloud-run-stdout-logging-hack-1iig). When you log to stdout, GCP's backend logging ingester handles all of that for you, free of charge.

If it determines that it is running in a GCP environment, slogcp further reduces the billable CPU cycles spent on JSON marshalling by:
 - **suppressing `log/slog`'s automatic timestamping** since the logging ingester will automatically timestamp logs anyway
 - **using shorter, single-letter aliases for `severity`**, which the logging ingester will recognize and convert to their standard values

#### Why not just use `cloud.google.com/go/logging` with `logging.RedirectAsJSON(os.Stdout)`?

`cloud.google.com/go/logging` has a built-in ability to JSONs to `stdout` rather than sending logs over the API, so why don't we just use that? Because `cloud.google.com/go/logging` is not logging-pattern agnostic

There are [three popular logging patterns](https://betterstack.com/community/guides/logging/logging-in-go/) when using [`log/slog`](https://pkg.go.dev/log/slog):
- **Global logging**: call `slog.Default` anywhere without wiring
- **Dependency-injected logging**: pass loggers explicitly to the components that need them
- **Request-scoped logging**: derive loggers from `context.Context` that carry per-request attributes

The official Cloud Logging client doesn't let you use any of these easily. `cloud.google.com/go/logging` ships its own `Logger` type and never implements the `slog.Handler` interface. That means, even if you're just logging to `stdout`, you cannot hand the client to `slog.SetDefault` for the global pattern, you cannot derive child loggers with `logger.With` for dependency-injected components, and you cannot swap in request-scoped loggers harvested from a `context.Context`. You could build an adapter that translates `slog.Record` into the client's `Entry` struct, but then you're re-creating a handler just to regain native `slog` ergonomics, and having to add the boilerplate to do so to each of your services.

Since slogcp is just a `slog.Handler`, you can use any logging pattern you can with `log/slog`. This also means slogcp easily be integrated with other popular slog libraries, like [m-mizutani/masq](https://github.com/m-mizutani/masq).

## Quick Start

```go
package main

import (
    "log"
    "log/slog"
    "os"

    "github.com/pjscruggs/slogcp"
)

func main() {
    // Create a handler with default settings
    handler, err := slogcp.NewHandler(os.Stdout)
    if err != nil {
        log.Fatalf("Failed to create handler: %v", err)
    }

    // No need to manually Close() when targeting stdout/stderr
    // For other targets (e.g., WithRedirectToFile),
    // `defer handler.Close()` to flush and release resources

    logger := slog.New(handler)
    // Log a simple message
    logger.Info("Application started")
}
```

## Configuration

slogcp allows most configurations to be applied both programmatically and through environmental variables. Programmatically applied settings are always given a higher priority than environmental variables.

For comprehensive configuration options, environment variables, and advanced usage patterns, please see our [Configuration Documentation](docs/CONFIGURATION.md).

### Opt-in Async Mode

`slogcp` stays fully synchronous by default. When you want to decouple callers from the write path, wrap the handler with the optional [`slogcpasync`](slogcpasync) package:

```go
handler, _ := slogcp.NewHandler(os.Stdout)
async := slogcpasync.Wrap(handler,
    slogcpasync.WithQueueSize(4096),
    slogcpasync.WithDropMode(slogcpasync.DropModeDropNewest),
)
logger := slog.New(async)
```

To control async settings via environment variables instead, combine `WithEnabled(false)` with `WithEnv()`:

```go
handler, _ := slogcp.NewHandler(os.Stdout,
    slogcp.WithMiddleware(
        slogcpasync.Middleware(
            slogcpasync.WithEnabled(false), // stay synchronous unless SLOGCP_ASYNC_* opts in
            slogcpasync.WithEnv(),
        ),
    ),
)
```

Supported variables:

- `SLOGCP_ASYNC_ENABLED`: `true`/`false` to turn the wrapper on.
- `SLOGCP_ASYNC_QUEUE_SIZE`: queue capacity (use `0` for an unbuffered queue).
- `SLOGCP_ASYNC_DROP_MODE`: `block` (default), `drop_newest`, or `drop_oldest`.
- `SLOGCP_ASYNC_WORKERS`: number of worker goroutines (defaults to `1`).
- `SLOGCP_ASYNC_FLUSH_TIMEOUT`: duration string for `Close` (for example, `5s`). 

### Core Configuration Options

If you don't want to read any more documentation right now, these are the configurations you're the most likely to care about. See [`.examples/configuration/main.go`](.examples/configuration/main.go) for a runnable demonstration that applies custom levels, source location, and default attributes.

`slogcp.Handler` also supports attribute rewriting via `slogcp.WithReplaceAttr(func(groups []string, attr slog.Attr) slog.Attr)`, which runs on every non-group attribute (including attributes added via `With`/`WithAttrs` and nested groups) before slogcp adds any of its own fields. Auto-added fields such as `severity`, `time`, `trace`, `serviceContext`, and `stack_trace` are appended after the replacer runs and therefore do not flow through this hook.

```go
handler, err := slogcp.NewHandler(os.Stdout,
    slogcp.WithReplaceAttr(func(groups []string, attr slog.Attr) slog.Attr {
        if attr.Key == "token" {
            return slog.String(attr.Key, "[redacted]")
        }
        return attr
    }),
)
```

### Environment Variables

Core environment variables for configuring slogcp:

| Variable | Description | Default |
| --- | --- | --- |
| `SLOGCP_TARGET` | `stdout`, `stderr`, or `file:/path` | `stdout` |
| `SLOGCP_LEVEL` | Minimum log level (`debug`, `info`, `warn`, `error`, etc.) | `info` |
| `SLOGCP_SOURCE_LOCATION` | Include source file/line (`true`, `false`) | `false` |
| `SLOGCP_STACK_TRACE_ENABLED` | Enable stack traces (`true`, `false`) | `false` |
| `SLOGCP_TRACE_DIAGNOSTICS` | Controls trace-correlation diagnostics: `off`, `warn`/`warn_once`, or `strict` | `warn_once` |

When the handler encounters attributes whose value is an `error`, it detects the first such attribute, records an `error_type`, and appends the error text to the log message so Error Reporting payloads remain informative. A `stack_trace` field is added when the error already carries a stack, or when stack traces are enabled via `SLOGCP_STACK_TRACE_ENABLED` or `slogcp.WithStackTraceEnabled` and the record's level meets the configured threshold.

If slogcp determines at runtime that it is running in a GCP environment such as Cloud Run, Cloud Functions, or App Engine and discovers service metadata, it will automatically populate `serviceContext` when it is missing so Error Reporting can group errors correctly without additional configuration.

### Dynamic Level Control

`slogcp.Handler` exposes runtime level tuning so you can raise or lower verbosity without redeploying. See [`.examples/dynamic-level/main.go`](.examples/dynamic-level/main.go) for a runnable example that tunes handler verbosity at runtime and shares a `slog.LevelVar` across components.

For multi-logger setups, pass a shared `*slog.LevelVar` via `slogcp.WithLevelVar` so every handler stays in sync.

### Error Reporting helpers

When you need fully Error Reporting-optimized entries, slogcp exposes helpers such as `slogcp.ErrorReportingAttrs(err)` and `slogcp.ReportError(...)`. These always attach Error Reporting-friendly fields like `serviceContext`, a Go-formatted `stack_trace`, and `reportLocation`, and accept overrides via `slogcp.WithErrorServiceContext(...)` and `slogcp.WithErrorMessage(...)`.

```go
logger.ErrorContext(ctx, "failed operation",
    append(
        []slog.Attr{slog.Any("error", err)},
        slogcp.ErrorReportingAttrs(err)...,
    )...,
)
```

## Examples

### In Google Cloud

See [`.examples/basic/main.go`](.examples/basic/main.go) for a minimal bootstrap that writes to stdout with slogcp.

## HTTP and gRPC Middleware

slogcp provides ready-to-use middleware for HTTP servers and gRPC services. The
HTTP helpers live in the `slogcphttp` package
(`github.com/pjscruggs/slogcp/slogcphttp`) and the gRPC interceptors live in the
`slogcpgrpc` package (`github.com/pjscruggs/slogcp/slogcpgrpc`).

Trace correlation reads the current OpenTelemetry span from the request context: the handler uses `trace.SpanContextFromContext` to obtain trace and span IDs and emits them as Cloud Logging-compatible fields. The HTTP middleware will either reuse an existing span, extract remote context via `otel.GetTextMapPropagator` (W3C `traceparent`/`tracestate`), or fall back to `X-Cloud-Trace-Context` if no span or W3C context is present, and by default wraps `otelhttp.NewHandler`, which uses the global tracer provider unless you override it. With a standard OpenTelemetry setup (global tracer provider and propagator), the logger automatically follows whatever span is active on the context.

### HTTP Example (Server)

See [`.examples/http-server/main.go`](.examples/http-server/main.go) for a runnable HTTP server that composes slogcp middleware with trace context injection.

### HTTP Example (Client propagation)

See [`.examples/http-client/main.go`](.examples/http-client/main.go) to watch the HTTP transport forward W3C trace context to downstream services.

### gRPC

- Client interceptors inject W3C `traceparent` (and optionally `X-Cloud-Trace-Context`) into outgoing metadata; server interceptors extract context for correlation.
- `ServerOptions` and `DialOptions` helpers bundle slogcp interceptors with OpenTelemetry instrumentation for streamlined registration.
- See [`.examples/grpc/main.go`](.examples/grpc/main.go) for a Greeter service that uses the interceptors end-to-end.

## Integration with other libraries

Since slogcp is just a `slog.Handler`, it can easily be integrated with other popular slog libraries.

### masq

Run an HTTP server that redacts sensitive request fields with `github.com/m-mizutani/masq` before logging via slogcp. See [.examples/masq/main.go](.examples/masq/main.go).

### lumberjack

Redirect slogcp output to a lumberjack rotating writer with `WithRedirectWriter` and optional reopen support. See [.examples/lumberjack/main.go](.examples/lumberjack/main.go).

## Advanced configuration

For gRPC interceptors and more advanced middleware options, see the [Configuration Documentation](docs/CONFIGURATION.md).

## License

[Apache 2.0](LICENSE)

## Contributing

Contributions are welcome! Feel free to submit issues for bugs or feature requests. For code contributions, please fork the repository, create a feature branch, and submit a pull request with your changes.
