# slogcp

<img src="logo.svg" width="50%" alt="slogcp logo">

A "batteries included" structured logging module for Google Cloud Platform with built-in HTTP and gRPC interceptors.

## Installation

```bash
go get github.com/pjscruggs/slogcp
```

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

## Why Would I Use This?

### Who is this for?

slogcp will be useful to you if you're using:

1. **these Google Cloud services:**
  - Cloud Run Services
  - Cloud Run Jobs
  - Cloud Functions
  - App Engine
  - Google Kubernetes Engine

2. **Google Cloud's native observability stack**  
  You rely on Cloud Logging, Cloud Trace, and Error Reporting to understand your services. So, you want logs, traces, and errors to line up cleanly in those UIs with correct severities, structured fields, trace IDs, and stack traces—without wiring all of that by hand in every service.

3. **You want to reduce the boilerplate**

In a typical Go service, you'd otherwise need to hand-roll a custom `slog.Handler` (or replacer) to rename `level` to `severity`, plumb OpenTelemetry span IDs into `logging.googleapis.com/trace` / `spanId` / `trace_sampled`, detect the current GCP runtime and project ID to populate `serviceContext.service` / `.version`, write HTTP and gRPC middleware to derive request-scoped loggers and attach method/route/status/latency/size fields, handle proxy-derived scheme/client IP metadata (for example `X-Forwarded-Proto`/`X-Forwarded-For`) on managed runtimes, and capture Go stack traces with `context.reportLocation` so Error Reporting can group crashes. slogcp packages all of that into one handler and a small set of middlewares, so each service just wires `slogcp.NewHandler`, `slogcphttp.Middleware`, and/or `slogcpgrpc.ServerOptions` and the client interceptors instead of re-implementing the same JSON shapes and trace/error wiring over and over again.


### Why not just use the official logging library?

#### Using `cloud.google.com/go/logging` is more expensive than logging to `stdout`

> [!NOTE]
> The official GCP documentation for the various services that support automatic ingestion of logs written to stdout/stderr doesn't have a consistent term for this feature. Lacking an official term, we'll be referring to this service as the "**logging ingester**."

CPU time is money. When you use a Cloud Logging client library and let it send logs to the Cloud Logging API, **your** billable service is responsible for marshaling every record into protobuf, maintaining gRPC streams, retrying transient failures, and batching writes across worker goroutines. If you don't configure the client correctly, [this can kill your performance](https://dev.to/siddhantkcode/2x-faster-40-less-ram-the-cloud-run-stdout-logging-hack-1iig). When you log to stdout, GCP's backend logging ingester handles all of that for you, free of charge.

If it determines that it is running in a GCP environment, slogcp further reduces the billable CPU cycles spent on JSON marshalling by:
 - **suppressing `log/slog`'s automatic timestamping** for stdout/stderr handlers since the logging ingester will automatically timestamp those entries (file targets keep timestamps so rotated/shipped logs stay annotated)
 - **using shorter, single-letter aliases for `severity`**, which the logging ingester will recognize and convert to their standard values

#### Why not just use `cloud.google.com/go/logging` with `logging.RedirectAsJSON(os.Stdout)`?

`cloud.google.com/go/logging` has a built-in ability to JSONs to `stdout` rather than sending logs over the API, so why don't we just use that?

Because, **`cloud.google.com/go/logging` is not logging-pattern agnostic**. It ships its own `Logger` type and never implements the `slog.Handler` interface. That means, even if you're just logging to `stdout`, you cannot hand the client to `slog.SetDefault` for a global pattern, you cannot derive child loggers with `logger.With` for a dependency-injected pattern, and you cannot swap in request-scoped loggers harvested for a request-scoped pattern. You could build an adapter that translates `slog.Record` into the client's `Entry` struct, but then you're re-creating a handler just to regain native `slog` ergonomics, and having to add the boilerplate to do so to each of your services.

## Features

### Severity fields
`log/slog` emits a vendor-neutral `level` field and leaves severity naming up to you, but Cloud Logging expects a `severity` field using its own enum. slogcp maps slog levels (including GCP-specific levels like `NOTICE`, `CRITICAL`, and `EMERGENCY`) onto the correct Cloud Logging severities and can emit single-letter aliases on managed GCP runtimes, so you don't need a custom JSON handler or `ReplaceAttr` function in every service.

### Trace correlation
Cloud Logging and Cloud Trace correlate logs via `logging.googleapis.com/trace`, `logging.googleapis.com/spanId`, and `logging.googleapis.com/trace_sampled`. slogcp reads the current OpenTelemetry span from context (covering W3C `traceparent`, `grpc-trace-bin`, and `X-Cloud-Trace-Context`) and populates these fields automatically. The HTTP and gRPC helpers also handle trace header extraction and injection for you, so logs come with clickable trace links in Logs Explorer without hand-rolled middleware.

### Error Reporting
Error Reporting groups errors by service and stack trace, but getting the JSON shape right (`serviceContext`, `stack_trace`, and `context.reportLocation`) is tedious. slogcp can capture Go stack traces, infer service metadata from Cloud Run/Functions/App Engine, and attach Error Reporting-friendly fields either automatically (based on level and configuration) or via helpers like `ErrorReportingAttrs` and `ReportError`, so plain `ERROR` logs become rich Error Reporting events without a separate client library.

### HTTP and gRPC Interceptors
HTTP and gRPC usually require bespoke middleware/interceptors just to get request-scoped loggers, consistent request/RPC attributes (method/route/status/duration/sizes), and trace context propagation. slogcp ships `slogcphttp` and `slogcpgrpc` helpers that do that wiring for you, so each service adds one middleware or `ServerOptions` call instead of re-implementing instrumentation.

### Pub/Sub Integration
Pub/Sub workflows usually require extra glue code: copy trace context into message attributes, recover it on the subscriber, derive a per-message logger, and remember to attach consistent subscription/topic/message fields so Logs Explorer stays queryable. slogcp’s `slogcppubsub` package (`github.com/pjscruggs/slogcp/slogcppubsub`) collapses that to a couple helpers (`Inject` and `WrapReceiveHandler`), giving you message-scoped loggers (`slogcp.Logger(ctx)`), Cloud Logging trace correlation, and OpenTelemetry messaging semantic-convention fields by default. It also supports optional consumer spans, `googclient_` trace attribute interop, and a “public endpoint” trust-boundary mode (new root + link) so you can keep end-to-end observability without blindly trusting producer trace IDs.

### Async Logging
`slogcp` writes synchronously to `stdout`/`stderr` by default. When slogcp writes to a file target (`SLOGCP_TARGET=file:...` or `slogcp.WithRedirectToFile`), it buffers writes by default so disk I/O doesn't sit on hot paths. See `docs/CONFIGURATION.md#async-logging-slogcpasync` to tune or disable buffering (or to opt into async for other targets).

> [!TIP]
> Async wrappers on `stdout`/`stderr` usually add overhead without improving throughput.

### Tested Out The Wazoo
slogcp has 100% local test coverage. Our testing process includes making sure all of our [examples](.examples) build and that their own tests pass. We also run a series of E2E tests **in Google Cloud** that spin up real Cloud Run services wired together with slogcp’s HTTP and gRPC interceptors. Those tests drive traffic through unary and streaming RPCs, then query Cloud Logging and Cloud Trace to verify severities, resource labels/serviceContext, and log names (`run.googleapis.com/stdout`), and that trace IDs/span IDs are correctly propagated so logs and spans from downstream HTTP and gRPC services all correlate into a single end‑to‑end trace in Google Cloud’s UIs.

### Easy compatibility with other slog libraries
Because slogcp is "just" a `slog.Handler` that writes JSON to an `io.Writer`, it slots into existing slog setups instead of replacing them. You still use `slog.New`, `slog.SetDefault`, `logger.With`, and request-scoped loggers, and you can compose slogcp with other slog-based tools like [masq](https://github.com/m-mizutani/masq) for redaction or [timberjack](https://github.com/DeRuina/timberjack/) (the maintained [lumberjack](https://github.com/natefinch/lumberjack) fork) for file rotation without special adapters. When you do write logs to files, the built-in `SwitchableWriter` and `Handler.ReopenLogFile` helpers let you cooperate with external rotation tools without rebuilding handlers or changing how the rest of your code logs.

## Core Configuration Options

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
| `SLOGCP_LEVEL` (fallback: `LOG_LEVEL`) | Minimum log level (`debug`, `info`, `warn`, `error`, etc.) | `info` |
| `SLOGCP_SOURCE_LOCATION` | Include source file/line (`true`, `false`) | `false` |
| `SLOGCP_STACK_TRACES` | Enable stack traces (`true`, `false`) | `false` |
| `SLOGCP_TRACE_DIAGNOSTICS` | Controls trace-correlation diagnostics: `off`, `warn`/`warn_once`, or `strict` | `warn_once` |

When the handler encounters attributes whose value is an `error`, it detects the first such attribute, records an `error_type`, and appends the error text to the log message so Error Reporting payloads remain informative. A `stack_trace` field is added when the error already carries a stack, or when stack traces are enabled via `SLOGCP_STACK_TRACES` or `slogcp.WithStackTraceEnabled` and the record's level meets the configured threshold.

If slogcp determines at runtime that it is running in a GCP environment such as Cloud Run, Cloud Functions, or App Engine and discovers service metadata, it will automatically populate `serviceContext` when it is missing so Error Reporting can group errors correctly without additional configuration.

### Dynamic Level Control

`slogcp.Handler` exposes runtime level tuning so you can raise or lower verbosity without redeploying. See [`.examples/dynamic-level/main.go`](.examples/dynamic-level/main.go) for a runnable example.

By default, each call to `slogcp.NewHandler` initializes its minimum level from `SLOGCP_LEVEL` (falling back to `LOG_LEVEL`). Override that programmatically with `slogcp.WithLevel`.

To change levels at runtime, call `handler.SetLevel(...)` for a single handler. If multiple handlers (or libraries) need to move together, share a single `*slog.LevelVar` via `slogcp.WithLevelVar` and update it once:

```go
levelVar := new(slog.LevelVar)

appHandler, err := slogcp.NewHandler(os.Stdout, slogcp.WithLevelVar(levelVar))
if err != nil {
	panic(err)
}

auditHandler, err := slogcp.NewHandler(nil, slogcp.WithRedirectToFile("audit.json"), slogcp.WithLevelVar(levelVar))
if err != nil {
	panic(err)
}
defer auditHandler.Close()

levelVar.Set(slog.LevelDebug) // app + audit now allow debug
```

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
- `ServerOptions` bundles slogcp interceptors with OpenTelemetry instrumentation for streamlined server registration; client code can use the provided interceptors directly.
- See [`.examples/grpc/main.go`](.examples/grpc/main.go) for a Greeter service that uses the interceptors end-to-end.
- If you're already invested in the [gRPC Ecosystem](https://github.com/grpc-ecosystem) ecosystem framework, slogcp still fits: use the ready-made [`slogcp-grpc-adapter`](https://github.com/pjscruggs/slogcp-grpc-adapter) module to have its logging interceptors emit slogcp/Cloud Logging–friendly JSON.

## Integration with other libraries

Since slogcp is just a `slog.Handler`, it can easily be integrated with other popular slog libraries.

### go-grpc-middleware

Using [`github.com/grpc-ecosystem/go-grpc-middleware`](https://github.com/grpc-ecosystem/go-grpc-middleware) for gRPC logging doesn’t block you from adopting slogcp. There’s a ready-made adapter, [`slogcp-grpc-adapter`](https://github.com/pjscruggs/slogcp-grpc-adapter), that plugs slogcp into its logging interceptors so you keep your existing interceptor chains while getting Cloud Logging–native JSON, trace correlation, and Error Reporting behavior.

### masq

Run an HTTP server that redacts sensitive request fields with `github.com/m-mizutani/masq` before logging via slogcp. See [.examples/masq/main.go](.examples/masq/main.go).

### timberjack

Redirect slogcp output to a timberjack rotating writer with `WithRedirectWriter` and optional reopen support. See [.examples/timberjack/main.go](.examples/timberjack/main.go).

## Advanced configuration

For more advanced middleware options, see the [Configuration Documentation](docs/CONFIGURATION.md).

## License

[Apache 2.0](LICENSE)

## Contributing

Contributions are welcome! Feel free to submit issues for bugs or feature requests. For code contributions, please fork the repository, create a feature branch, and submit a pull request with your changes.
