# slogcp

<img src="logo.svg" width="50%" alt="slogcp logo">

A "batteries included" structured logging module for Google Cloud with built-in HTTP and gRPC interceptors.

## Installation

```bash
go get github.com/pjscruggs/slogcp
```

## Features

## Features

- {🪵} **Structured JSON logging** for powerful filtering and analysis in Cloud Logging
-  ☁️  **GCP Cloud Logging API integration** for increased reliability and throughput over `stdout` / `stderr`
-  🌈  **Complete GCP severity level support** (DEBUG, INFO, NOTICE, WARNING, ERROR, CRITICAL, ALERT, EMERGENCY)
-  📡  **Automatic trace context extraction** that correlates logs with Cloud Trace spans with zero configuration
-  🧩  **Ready-to-use HTTP and gRPC middleware** with optimized GCP-friendly log structuring
-  🎚️  **Dynamic log level control** without application restart
-  🐛  **Error logging with optional stack traces** for efficient debugging
-  🏷️  **Automatic Cloud Run metadata detection** for proper resource association
-  🪂  **Graceful shutdown handling** with automatic buffered log flushing

# Configuration

slogcp offers multiple ways to configure logging behavior, allowing both programmatic control and environment variable settings.

## Configuration Options

Configure the logger by passing `Option` functions to `New()`:

```go
logger, err := slogcp.New(
    slogcp.WithLevel(slog.LevelDebug),
    slogcp.WithSourceLocationEnabled(true),
    slogcp.WithStackTraceEnabled(true),
)
```

### Core Options

| Option | Description | Default |
|--------|-------------|---------|
| `WithProjectID(id string)` | Sets Google Cloud Project ID | Auto-detected or from env |
| `WithLevel(level slog.Level)` | Sets minimum logging level | `slog.LevelInfo` |
| `WithSourceLocationEnabled(bool)` | Include source file/line in logs | `false` |
| `WithAttrs([]slog.Attr)` | Add default attributes to all logs | `nil` |
| `WithGroup(name string)` | Add a default group to all logs | `""` |

### Stack Trace Options

| Option | Description | Default |
|--------|-------------|---------|
| `WithStackTraceEnabled(bool)` | Enable automatic stack traces | `false` |
| `WithStackTraceLevel(level slog.Level)` | Minimum level for stack traces | `slog.LevelError` |

### Cloud Logging Client Options

| Option | Description | Default |
|--------|-------------|---------|
| `WithEntryCountThreshold(count int)` | Max entries to buffer before sending | Client library default |
| `WithDelayThreshold(delay time.Duration)` | Max time to buffer entries | Client library default |
| `WithMonitoredResource(res *monitoredres.MonitoredResource)` | Explicitly set Cloud Logging resource | Auto-detected |
| `WithCloudRunPayloadAttributes(bool)` | Add Cloud Run service/revision to logs | `false` |

## Environment Variables

Configuration can also be set via environment variables:

| Variable | Description | Default |
|----------|-------------|---------|
| `GOOGLE_CLOUD_PROJECT` | GCP project ID (required if not auto-detected) | Auto-detected on GCP |
| `LOG_LEVEL` | Minimum log level (named or numeric: `DEBUG`/`-4`, `INFO`/`0`, `ERROR`/`8`, etc.) | `INFO` |
| `LOG_SOURCE_LOCATION` | Enable source location (`true`, `1`) | `false` |
| `LOG_STACK_TRACE_ENABLED` | Enable stack traces (`true`, `1`) | `false` |
| `LOG_STACK_TRACE_LEVEL` | Minimum level for stack traces (named or numeric) | `ERROR` |
| `K_SERVICE` | Cloud Run service name | Auto-set in Cloud Run |
| `K_REVISION` | Cloud Run revision name | Auto-set in Cloud Run |

Options provided programmatically override environment variables.

## GCP Project ID Resolution

The project ID is determined in this order:
1. Value provided via `WithProjectID()`
2. `GOOGLE_CLOUD_PROJECT` environment variable
3. Automatic detection via GCP metadata server (if running on GCP)

## Basic Usage

Here's a basic example for a REST server for a specialy food shop, with a single inventory-checking endpoint.

### Initialization

```go
package main

import (
	"context"
	"log"
	"net/http"

	"github.com/pjscruggs/slogcp"
	slogcphttp "github.com/pjscruggs/slogcp/http"
)

// logger is the application-wide slogcp logger.
// Declared at package level for access by handlers.
var logger *slogcp.Logger

var inventory map[string]int

func main() {
	ctx := context.Background()
	var err error

	// Create the slogcp logger instance.
	logger, err = slogcp.New()
	if err != nil {
		log.Fatalf("Failed to create slogcp logger: %v", err)
	}

	// Ensure logs are flushed on exit.
	defer logger.Close()

	logger.InfoContext(ctx, "Cheese shop inventory system starting up")

	// Initialize inventory
	initializeInventory(ctx)

	// Set up HTTP handler with slogcp middleware for request logging
	checkHandler := slogcphttp.Middleware(logger.Logger)(http.HandlerFunc(handleCheckInventory))
	http.Handle("/check", checkHandler)

	// Start the HTTP server
	port := ":8080"
	logger.InfoContext(ctx, "Starting HTTP server", "address", port)
	if err := http.ListenAndServe(port, nil); err != nil {
		logger.ErrorContext(ctx, "HTTP server failed", "error", err)
		log.Fatal(err) // Use standard log.Fatal for critical startup errors
	}

	
	logger.InfoContext(ctx, "Cheese shop inventory system shutting down")
}

```

### Logging in Application Logic

```go
package main

import (
	"context"
	"fmt"
	"log/slog"
)

// initializeInventory sets up the initial state of the cheese inventory.
func initializeInventory(ctx context.Context) {

	varieties := []string{
		"Red Leicester", "Tilsit", "Caerphilly", "Bel Paese", "Red Windsor",
		"Stilton", "Gruyère", "Emmental", "Norwegian Jarlsberg", "Liptauer",
		"Lancashire", "White Stilton", "Danish Blue", "Double Gloucester",
		"Cheshire", "Dorset Blue Vinney", "Brie", "Roquefort", "Pont l'Évêque",
		"Port Salut", "Savoyard", "Saint-Paulin", "Carré de l'Est", "Boursin",
		"Bresse-Bleu", "Perle de Champagne", "Camembert", "Gouda", "Edam",
		"Caithness", "Smoked Austrian", "Sage Derby", "Wensleydale",
		"Gorgonzola", "Parmesan", "Mozzarella", "Pipo Crem", "Danish Fynbo",
		"Czechoslovakian sheep's milk", "Venezuelan Beaver Cheese", "Cheddar",
		"Ilchester", "Limburger",
	}


	inventory = make(map[string]int)
	for _, variety := range varieties {
		inventory[variety] = 0
	}
	logger.DebugContext(ctx, "Initialized inventory", slog.Int("variety_count", len(inventory)))
}

// CheckInventory returns the current stock level of a cheese variety and an error if the requested cheese is unknown.
func CheckInventory(ctx context.Context, cheeseName string) (int, error) {
	
    var err error
	stock, known := inventory[cheeseName]

	if !known {
		logger.WarnContext(ctx, "Unknown product variety requested",
			slog.String("requested_cheese", cheeseName),
		)

		err = fmt.Errorf("No.")
	} else if stock <= 0 {
		logger.InfoContext(ctx, "Requested cheese variety out of stock.",
			slog.String("cheese_name", cheeseName),
			slog.Int("stock_level", stock),
		)

	}

	return stock, err

	logger.EmergencyContext(ctx, "Found cheese in stock!",
		slog.String("cheese_name", cheeseName),
		slog.Int("stock_found", stock),
	)
}

```
