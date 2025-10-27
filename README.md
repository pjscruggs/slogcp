# slogcp

<img src="logo.svg" width="50%" alt="slogcp logo">

A "batteries included" structured logging module for Google Cloud Platform with built-in HTTP and gRPC interceptors.

## ⚠️ EXPERIMENTAL: DO NOT USE IN PRODUCTION ⚠️

This module is untested, and not recommended for any production use.

I am currently working on creating end-to-end tests which will run in Google Cloud.

## Installation

```bash
go get github.com/pjscruggs/slogcp
```

## Features

* {🪵} **Structured JSON logging** for powerful filtering and analysis in Cloud Logging
* ☁️ **Cloud Logging-compatible JSON formatting** automatically shapes every entry for Google Cloud ingestion
* 🌈  **Complete GCP severity level support** (DEBUG, INFO, NOTICE, WARNING, ERROR, CRITICAL, ALERT, EMERGENCY)
* 📡  **Automatic trace context extraction and propagation** (gRPC by default; optional HTTP client transport)
* 🚚  **Optional HTTP client transport** that injects W3C Trace Context (and optionally `X-Cloud-Trace-Context`) on outbound requests
* 🧩  **Ready-to-use HTTP and gRPC middleware** with optimized GCP-friendly log structuring
* 🎚️  **Dynamic log level control** without application restart
* 🐛  **Error logging with optional stack traces** for efficient debugging
* 🏷️  **Automatic GCP resource detection** for proper log association

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
    defer handler.Close() // Always call Close() to flush buffered logs

    logger := slog.New(handler)
	// Log a simple message
	logger.Info("Application started")
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
    
    // Add common attributes to all logs
    slogcp.WithAttrs([]slog.Attr{slog.String("service", "user-api")}),
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
| `SLOGCP_LOG_TARGET`       | Legacy redirect hint: `stdout`, `stderr`, or `file`        | `stdout` |
| `SLOGCP_REDIRECT_AS_JSON_TARGET` | Preferred redirect target (`stdout`, `stderr`, `file:/path`) | (none) |
| `LOG_LEVEL`               | Minimum log level (`debug`, `info`, `warn`, `error`, etc.) | `info`  |
| `LOG_SOURCE_LOCATION`     | Include source file/line (`true`, `false`)                 | `false` |
| `LOG_TIME` *(alias `LOG_TIME_FIELD_ENABLED`)* | Emit RFC3339Nano `time` field (`slogcp.WithTime(true)` programmatic equivalent) | `false` |
| `LOG_STACK_TRACE_ENABLED` | Enable stack traces (`true`, `false`)                      | `false` |

### Dynamic Level Control

`slogcp.Handler` exposes runtime level tuning so you can raise or lower verbosity without redeploying:

```go
handler, _ := slogcp.NewHandler(os.Stdout)
logger := slog.New(handler)

handler.SetLevel(slog.LevelWarn) // drop info/debug noise globally
// or share control with another component:
levelVar := handler.LevelVar()
levelVar.Set(slog.LevelDebug)

logger.Debug("now enabled")
```

For multi-logger setups, pass a shared `*slog.LevelVar` via `slogcp.WithLevelVar` so every handler stays in sync.

## Common Usage Patterns

### In Google Cloud

```go
// In Cloud Run, GCE, GKE, etc., stdout/stderr are collected by Cloud Logging automatically.
handler, err := slogcp.NewHandler(os.Stdout)
if err != nil {
    log.Fatalf("failed to create handler: %v", err)
}
defer handler.Close()

logger := slog.New(handler)
logger.Info("service ready")
```

### Local Development

#### Explicit Local Configuration

```go
// Explicitly configure for local development
handler, err := slogcp.NewHandler(os.Stdout,
    slogcp.WithLevel(slog.LevelDebug),
    slogcp.WithSourceLocationEnabled(true), // Helpful for debugging
)
if err != nil {
    log.Fatalf("failed to create handler: %v", err)
}
defer handler.Close()

logger := slog.New(handler)
logger.Debug("local logger ready")
```

## HTTP and gRPC Middleware

slogcp provides ready-to-use middleware for HTTP servers and gRPC services.

### HTTP Example (Server)

```go
import (
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
        log.Fatalf("failed to create slogcp handler: %v", err)
    }
    defer handler.Close()

    logger := slog.New(handler)

    // Apply middleware to your handler
    wrapped := slogcphttp.Middleware(logger)(
        slogcphttp.InjectTraceContextMiddleware()(http.HandlerFunc(myHandler)),
    )

    http.Handle("/api", wrapped)
    if err := http.ListenAndServe(":8080", nil); err != nil {
        logger.Error("server stopped", slog.String("error", err.Error()))
    }
}
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
* “Manual” injectors (`InjectUnaryTraceContextInterceptor` / `InjectStreamTraceContextInterceptor`) are available for servers **without** OTel propagation configured.

For gRPC interceptors and more advanced middleware options, see the [Configuration Documentation](docs/CONFIGURATION.md).

## License

[Apache 2.0](LICENSE)

## Contributing

Contributions are welcome! Feel free to submit issues for bugs or feature requests. For code contributions, please fork the repository, create a feature branch, and submit a pull request with your changes.