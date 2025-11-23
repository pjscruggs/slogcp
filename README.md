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

- {🪵}  **Structured JSON logging** for powerful filtering and analysis in Cloud Logging
- ☁️ **Cloud Logging-compatible JSON formatting** automatically shapes every entry for Google Cloud ingestion
- 🎚️ **Complete GCP severity level support** (DEBUG, INFO, NOTICE, WARNING, ERROR, CRITICAL, ALERT, EMERGENCY)
- 🔗 **Automatic trace context extraction and propagation** (gRPC interceptors and HTTP middleware/transport)
- 🚚 **Optional HTTP client transport** that injects W3C Trace Context (and optionally `X-Cloud-Trace-Context`) on outbound requests
- 🧩 **Ready-to-use HTTP and gRPC middleware** with optimized GCP-friendly log structuring
- 🎛️ **Dynamic log level control** without application restart
- 🐛 **Error logging with optional stack traces** for efficient debugging
- 📡 **Automatic GCP resource detection** for proper log association

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

If you don't want to read any more documentation right now, these are the configurations you're the most likely to care about. See [`.examples/configuration/main.go`](.examples/configuration/main.go) for a runnable demonstration that applies custom levels, source location, and default attributes.

### Environment Variables

Core environment variables for configuring slogcp:

| Variable | Description | Default |
| --- | --- | --- |
| `SLOGCP_TARGET` | `stdout`, `stderr`, or `file:/path` | `stdout` |
| `SLOGCP_LEVEL` | Minimum log level (`debug`, `info`, `warn`, `error`, etc.) | `info` |
| `SLOGCP_SOURCE_LOCATION` | Include source file/line (`true`, `false`) | `false` |
| `SLOGCP_TIME` | Emit RFC3339Nano `time` field (`slogcp.WithTime(true)` programmatic equivalent) | `false` |
| `SLOGCP_STACK_TRACE_ENABLED` | Enable stack traces (`true`, `false`) | `false` |
| `SLOGCP_TRACE_DIAGNOSTICS` | Controls trace-correlation diagnostics: `off`, `warn`/`warn_once`, or `strict` | `warn_once` |

### Dynamic Level Control

`slogcp.Handler` exposes runtime level tuning so you can raise or lower verbosity without redeploying. See [`.examples/dynamic-level/main.go`](.examples/dynamic-level/main.go) for a runnable example that tunes handler verbosity at runtime and shares a `slog.LevelVar` across components.

For multi-logger setups, pass a shared `*slog.LevelVar` via `slogcp.WithLevelVar` so every handler stays in sync.

## Common Usage Patterns

### In Google Cloud

See [`.examples/basic/main.go`](.examples/basic/main.go) for a minimal bootstrap that writes to stdout with slogcp.

## HTTP and gRPC Middleware

slogcp provides ready-to-use middleware for HTTP servers and gRPC services.

### HTTP Example (Server)

See [`.examples/http-server/main.go`](.examples/http-server/main.go) for a runnable HTTP server that composes slogcp middleware with trace context injection.

### HTTP Example (Client propagation)

See [`.examples/http-client/main.go`](.examples/http-client/main.go) to watch the HTTP transport forward W3C trace context to downstream services.

### gRPC

- Client interceptors inject W3C `traceparent` (and optionally `X-Cloud-Trace-Context`) into outgoing metadata; server interceptors extract context for correlation.
- `ServerOptions` and `DialOptions` helpers bundle slogcp interceptors with OpenTelemetry instrumentation for streamlined registration.
- See [`.examples/grpc/main.go`](.examples/grpc/main.go) for a Greeter service that uses the interceptors end-to-end.

For rotation-friendly logging backed by [lumberjack](https://github.com/natefinch/lumberjack), see [`.examples/lumberjack/main.go`](.examples/lumberjack/main.go) and its accompanying test.

For a more advanced demonstration that redacts sensitive tokens, check out [`.examples\masq\main.go`](.examples/masq/main.go).

For gRPC interceptors and more advanced middleware options, see the [Configuration Documentation](docs/CONFIGURATION.md).

## License

[Apache 2.0](LICENSE)

## Contributing

Contributions are welcome! Feel free to submit issues for bugs or feature requests. For code contributions, please fork the repository, create a feature branch, and submit a pull request with your changes.
