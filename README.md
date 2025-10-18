# slogcp

<img src="logo.svg" width="50%" alt="slogcp logo">

A "batteries included" structured logging module for Google Cloud Platform with built-in HTTP and gRPC interceptors.

## WARNING: EXPERIMENTAL - DO NOT USE IN PRODUCTION

This module is untested, and not recommended for any production use.

I am currently working on creating end-to-end tests which will run in Google Cloud.

## Installation

```bash
go get github.com/pjscruggs/slogcp
```

## Features

- **Structured JSON logging** for powerful filtering and analysis in Cloud Logging
- **Complete GCP severity level support** (DEBUG, INFO, NOTICE, WARNING, ERROR, CRITICAL, ALERT, EMERGENCY)
- **Automatic trace context extraction and propagation** (gRPC by default; optional HTTP client transport)
- **Optional HTTP client transport** that injects W3C Trace Context (and optionally `X-Cloud-Trace-Context`) on outbound requests
- **Ready-to-use HTTP and gRPC middleware** with optimized GCP-friendly log structuring
- **Shared health-check filter** that can tag, demote, or drop Cloud Run / load-balancer probes without custom conditionals
- **Request-scoped loggers** via `slogcp.Logger(ctx)` so handlers can add rich context without plumbing
- **Configurable log level via options or `LOG_LEVEL`**
- **Error logging with optional stack traces** for efficient debugging
- **Cloud Error Reporting helper** to emit `serviceContext`, stack traces, and report locations in one call
- **Optional GCP Cloud Logging API integration** when you need increased reliability and throughput over `stdout` / `stderr`
- **Configurable GCP resource and label support** for proper log association
- **Automatic Cloud Run/Functions metadata** added to stdout/stderr JSON payloads for parity with the Logging API
- **Smart environment fallback** that keeps stdout/stderr as the default target while offering GCP Cloud Logging when requested
- **Graceful shutdown handling** with automatic buffered log flushing

## Quick Start

```go
package main

import (
	"context"
	"log"
	"log/slog"
	"os"

	"github.com/pjscruggs/slogcp"
)

func main() {
	handler, err := slogcp.NewHandler(os.Stdout)
	if err != nil {
		log.Fatalf("Failed to create handler: %v", err)
	}
	defer handler.Close() // Always call Close() to flush buffered logs

	logger := slog.New(handler)
	logger.InfoContext(context.Background(), "Application started")
}
```

## Configuration

slogcp allows most configurations to be applied both programmatically and through environmental variables. Programmatically applied settings are always given a higher priority than environmental variables.

For comprehensive configuration options, environment variables, and advanced usage patterns, please see our [Configuration Documentation](docs/CONFIGURATION.md).

### Core Configuration Options

If you don't want to read any more documentation right now, these are the configurations you're the most likely to care about:

```go
handler, err := slogcp.NewHandler(os.Stdout,
    // Set minimum log level
    slogcp.WithLevel(slog.LevelDebug),
    
    // Enable source code location (file:line)
    slogcp.WithSourceLocationEnabled(true),
    
    // Add common labels to all logs
    slogcp.WithGCPCommonLabel("service", "user-api"),
)
if err != nil {
    log.Fatalf("failed to create handler: %v", err)
}
logger := slog.New(handler)
```

### GCP Logging Client Access

slogcp exposes all the underlying Google Cloud Logging client's configurables through passthrough options. Most of these can be configured both programmatically and with environmental variables.

```go
handler, err := slogcp.NewHandler(os.Stdout,
    // Example: Tune buffering parameters
    slogcp.WithGCPEntryCountThreshold(500),
    slogcp.WithGCPDelayThreshold(time.Second * 2),
)
if err != nil {
    log.Fatalf("failed to create handler: %v", err)
}
logger := slog.New(handler)
```

### Environment Variables

Core environment variables for configuring slogcp:

| Variable                  | Description                                                | Default |
| ------------------------- | ---------------------------------------------------------- | ------- |
| `SLOGCP_LOG_TARGET`       | Where to send logs: `gcp`, `stdout`, `stderr`, `file`      | `stdout`   |
| `LOG_LEVEL`               | Minimum log level (`debug`, `info`, `warn`, `error`, etc.) | `info`  |
| `LOG_SOURCE_LOCATION`     | Include source file/line (`true`, `false`)                 | `false` |
| `LOG_STACK_TRACE_ENABLED` | Enable stack traces (`true`, `false`)                      | `false` |

## Common Usage Patterns

### In Google Cloud

```go
// In Cloud Run, Cloud Functions, App Engine, and GKE stdout/stderr are ingested by Cloud Logging automatically
handler, err := slogcp.NewHandler(os.Stdout) // Defaults to stdout; no extra configuration required
if err != nil {
    log.Fatalf("failed to create handler: %v", err)
}
defer handler.Close()

logger := slog.New(handler)
logger.Info("service ready")
```

```go
// On GCE you can opt into the Cloud Logging API client for direct ingestion
handler, err := slogcp.NewHandler(os.Stdout,
    slogcp.WithLogTarget(slogcp.LogTargetGCP),
    slogcp.WithProjectID("my-project"),
)
if err != nil {
    log.Fatalf("failed to create handler: %v", err)
}
defer handler.Close()

logger := slog.New(handler)
```

### Local Development

#### Automatic Fallback

```go
handler, err := slogcp.NewHandler(os.Stdout)
if err != nil {
    log.Fatalf("failed to create handler: %v", err)
}
defer handler.Close()

if handler.IsInFallbackMode() {
    slog.New(handler).Debug("Automatic fallback to local logging is active")
}
```

#### Explicit Local Configuration

```go
handler, err := slogcp.NewHandler(os.Stdout,
    slogcp.WithLevel(slog.LevelDebug),
    slogcp.WithSourceLocationEnabled(true), // Helpful for debugging
)
if err != nil {
    log.Fatalf("failed to create handler: %v", err)
}
defer handler.Close()

logger := slog.New(handler)
```

## HTTP and gRPC Middleware

slogcp provides ready-to-use middleware for HTTP servers and gRPC services.

### HTTP Example (Server)

```go
import (
    "context"
    "log"
    "log/slog"
    "net/http"
    "os"

    "github.com/pjscruggs/slogcp"
    slogcphttp "github.com/pjscruggs/slogcp/http"
)

func main() {
    handler, err := slogcp.NewHandler(os.Stdout)
    if err != nil {
        log.Fatalf("failed to create handler: %v", err)
    }
    defer handler.Close()

    logger := slog.New(handler)
    wrapped := slogcphttp.Middleware(logger)(
        slogcphttp.InjectTraceContextMiddleware()(
            http.HandlerFunc(myHandler),
        ),
    )

    http.Handle("/api", wrapped)
    if err := http.ListenAndServe(":8080", nil); err != nil {
        logger.ErrorContext(context.Background(), "HTTP server failed", slog.String("error", err.Error()))
    }
}

func myHandler(w http.ResponseWriter, r *http.Request) {
    // Grab the per-request logger populated by the middleware.
    slogcp.Logger(r.Context()).Info("handling request")
    w.WriteHeader(http.StatusOK)
}
```

#### Health-check filtering

Use the shared filter to demote or drop noisy probes without bespoke conditionals:

```go
import (
    slogcphttp "github.com/pjscruggs/slogcp/http"
    "github.com/pjscruggs/slogcp/healthcheck"
)

hc := healthcheck.DefaultConfig()
hc.Enabled = true
hc.Mode = healthcheck.ModeDrop
hc.Paths = []string{"/healthz", "/_ah/health"}

wrapped := slogcphttp.Middleware(logger, slogcphttp.WithHealthCheckFilter(hc))(http.HandlerFunc(myHandler))
```

### HTTP Example (Client propagation)

```go
import (
    "net/http"
    slogcphttp "github.com/pjscruggs/slogcp/http"
)

// Create an HTTP client that forwards trace context from req.Context()
client := &http.Client{
    Transport: slogcphttp.NewTraceRoundTripper(nil), // wraps http.DefaultTransport
}

// In a handler where r.Context() carries inbound trace context:
req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, "https://downstream", nil)
resp, err := client.Do(req)
```

### gRPC

* Client interceptors inject W3C `traceparent` into outgoing metadata by default; server interceptors extract context for correlation.
* "Manual" injectors (`InjectUnaryTraceContextInterceptor` / `InjectStreamTraceContextInterceptor`) are available for servers **without** OTel propagation configured.

For gRPC interceptors and more advanced middleware options, see the [Configuration Documentation](docs/CONFIGURATION.md).

### Error reporting helper

Use `slogcp.ReportError` (or `slogcp.ErrorReportingAttrs`) to emit Cloud Error Reporting friendly payloads without hand-crafting `serviceContext`, `context.reportLocation`, or stack traces.

```go
if err := svc.Save(ctx, input); err != nil {
    slogcp.ReportError(ctx, logger, err, "Save failed")
}
```

## License

[Apache 2.0](LICENSE)

## Contributing

Contributions are welcome! Feel free to submit issues for bugs or feature requests. For code contributions, please fork the repository, create a feature branch, and submit a pull request with your changes.
