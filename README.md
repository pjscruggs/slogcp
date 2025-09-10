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
* ☁️  **GCP Cloud Logging API integration** for increased reliability and throughput over `stdout` / `stderr`
* 🌈  **Complete GCP severity level support** (DEBUG, INFO, NOTICE, WARNING, ERROR, CRITICAL, ALERT, EMERGENCY)
* 📡  **Automatic trace context extraction and propagation** (gRPC by default; optional HTTP client transport)
* 🚚  **Optional HTTP client transport** that injects W3C Trace Context (and optionally `X-Cloud-Trace-Context`) on outbound requests
* 🧩  **Ready-to-use HTTP and gRPC middleware** with optimized GCP-friendly log structuring
* 🎚️  **Dynamic log level control** without application restart
* 🐛  **Error logging with optional stack traces** for efficient debugging
* 🏷️  **Automatic GCP resource detection** for proper log association
* 🔄  **Smart environment detection** that automatically falls back to local logging when needed
* 🪂  **Graceful shutdown handling** with automatic buffered log flushing

## Quick Start

```go
package main

import (
	"context"
	"log"

	"github.com/pjscruggs/slogcp"
)

func main() {
	// Create a logger with default settings
	logger, err := slogcp.New()
	if err != nil {
		log.Fatalf("Failed to create logger: %v", err)
	}
	defer logger.Close() // Always call Close() to flush buffered logs

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
logger, err := slogcp.New(
    // Set minimum log level
    slogcp.WithLevel(slog.LevelDebug),
    
    // Enable source code location (file:line)
    slogcp.WithSourceLocationEnabled(true),
    
    // Add common labels to all logs
    slogcp.WithGCPCommonLabel("service", "user-api"),
)
```

### GCP Logging Client Access

slogcp exposes all the underlying Google Cloud Logging client's configurables through passthrough options. Most of these can be configured both programmatically and with environmental variables.

```go
logger, err := slogcp.New(
    // Example: Tune buffering parameters
    slogcp.WithGCPEntryCountThreshold(500),
    slogcp.WithGCPDelayThreshold(time.Second * 2),
)
```

### Environment Variables

Core environment variables for configuring slogcp:

| Variable                  | Description                                                | Default |
| ------------------------- | ---------------------------------------------------------- | ------- |
| `SLOGCP_LOG_TARGET`       | Where to send logs: `gcp`, `stdout`, `stderr`, `file`      | `gcp`   |
| `LOG_LEVEL`               | Minimum log level (`debug`, `info`, `warn`, `error`, etc.) | `info`  |
| `LOG_SOURCE_LOCATION`     | Include source file/line (`true`, `false`)                 | `false` |
| `LOG_STACK_TRACE_ENABLED` | Enable stack traces (`true`, `false`)                      | `false` |

## Common Usage Patterns

### In Google Cloud

```go
// In Cloud Run, GCE, GKE, etc., logs automatically go to Cloud Logging
logger, err := slogcp.New() // No options needed for default GCP behavior

// Or with additional options
logger, err := slogcp.New(
    slogcp.WithLevel(slog.LevelInfo),
    slogcp.WithGCPCommonLabel("service", "user-api"),
)
```

### Local Development

#### Automatic Fallback

```go
// Automatic fallback to stdout when GCP credentials aren't available
logger, err := slogcp.New()
if logger.IsInFallbackMode() {
    // Customize local development behavior if needed
    logger.SetLevel(slog.LevelDebug)
}
```

#### Explicit Local Configuration

```go
// Explicitly configure for local development
logger, err := slogcp.New(
    slogcp.WithRedirectToStdout(),
    slogcp.WithLevel(slog.LevelDebug),
    slogcp.WithSourceLocationEnabled(true), // Helpful for debugging
)
```

## HTTP and gRPC Middleware

slogcp provides ready-to-use middleware for HTTP servers and gRPC services.

### HTTP Example (Server)

```go
import (
    "net/http"
    
    "github.com/pjscruggs/slogcp"
    slogcphttp "github.com/pjscruggs/slogcp/http"
)

func main() {
    logger, _ := slogcp.New()
    defer logger.Close()
    
    // Apply middleware to your handler
    handler := slogcphttp.Middleware(logger.Logger)(
        slogcphttp.InjectTraceContextMiddleware()(
            http.HandlerFunc(myHandler),
        ),
    )
    
    http.Handle("/api", handler)
    http.ListenAndServe(":8080", nil)
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
    Transport: slogcphttp.NewPropagatingTransport(nil), // wraps http.DefaultTransport
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
