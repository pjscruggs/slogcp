# slogcp

<img src="logo.svg" width="50%" alt="slogcp logo">

GCP-native structured logging for Go (slog) microservices on Cloud Run, with built-in gRPC interceptors.

## Installation

```bash
go get github.com/pjscruggs/slogcp
```



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


## Features

*   Formats log entries as structured JSON suitable for querying in Cloud Logging.
*   Sends logs directly and asynchronously to the Google Cloud Logging API.
*   Includes OpenTelemetry trace and span IDs in log entries when present in the context, enabling correlation in Cloud Trace.
*   Formats logged Go errors for automatic recognition by Cloud Error Reporting.
*   Optionally includes stack traces with error logs for improved debugging in Error Reporting.
*   Relies on the underlying Cloud Logging client library to associate logs with the correct monitored resource (like a Cloud Run service and revision).
*   Provides convenience methods for logging at all standard Google Cloud Logging severity levels (Default, Debug, Info, Notice, Warning, Error, Critical, Alert, Emergency).
*   Optionally includes the source code file, line number, and function name where a log message originated.
*   Offers gRPC server interceptors to automatically log details about RPC calls (service, method, duration, status, peer address, errors) and correlate logs with traces.
*   Provides standard `net/http` middleware to log details about HTTP requests (method, path, status code, latency, size, remote IP, user agent) and correlate logs with traces, populating the `httpRequest` field in Cloud Logging.
*   Allows dynamic adjustment of the minimum logging level after the logger is initialized.

## Configuration

You can configure the logger using environment variables or functional options passed to `slogcp.New`. Functional options always override environment variables and auto-detection mechanisms.

### Environment Variables

| Variable Name             | Description                                                                                                                                                                                                                                                        | Default Value    |
| :------------------------ | :----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | :--------------- |
| `LOG_LEVEL`               | Sets the minimum log level. Accepts standard `slog` levels (`DEBUG`, `INFO`, `WARN`, `ERROR`) and extended GCP levels (`DEFAULT`, `NOTICE`, `CRITICAL`, `ALERT`, `EMERGENCY`), case-insensitive.                                                                       | `INFO`           |
| `LOG_SOURCE_LOCATION`     | If set to `true` or `1`, includes the source code file, line number, and function name in log entries. Enabling this incurs a performance cost.                                                                                                                     | `false`          |
| `LOG_STACK_TRACE_ENABLED` | If set to `true` or `1`, enables capturing stack traces for logs at or above the `LOG_STACK_TRACE_LEVEL`. Enabling this incurs a performance cost.                                                                                                                  | `false`          |
| `LOG_STACK_TRACE_LEVEL`   | Sets the minimum log level for capturing stack traces when `LOG_STACK_TRACE_ENABLED` is true. Accepts the same level names as `LOG_LEVEL`.                                                                                                                            | `ERROR`          |
| `GOOGLE_CLOUD_PROJECT`    | Sets the Google Cloud project ID. This variable is checked if the [`WithProjectID`](#functional-options) option isn't used, and it overrides automatic detection. Since the ID is usually detected automatically on Cloud Run or other GCP infrastructure (via the metadata server), you often won't need this in production. It's most helpful for local development or to force a specific ID instead of the detected one. If no project ID can be found (from the option, this variable, or auto-detection), the setup fails. | (Auto-detected) |


### Functional Options

Pass these to `slogcp.New` to configure the logger programmatically. They take precedence over environment variables and auto-detection.

*   `slogcp.WithProjectID(string)`: Explicitly sets the Google Cloud Project ID, overriding environment variables and auto-detection.
    ```go
    logger, err := slogcp.New(slogcp.WithProjectID("my-gcp-project-id"))
    ```
*   `slogcp.WithMonitoredResource(*mrpb.MonitoredResource)`: Explicitly sets the Monitored Resource for log entries, overriding the client library's auto-detection. Useful if auto-detection is insufficient or incorrect.
    ```go
    import mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
    // ...
    res := &mrpb.MonitoredResource{
        Type: "gce_instance",
        Labels: map[string]string{
            "project_id":  "my-gcp-project-id",
            "instance_id": "my-instance-id",
            "zone":        "us-central1-a",
        },
    }
    logger, err := slogcp.New(slogcp.WithMonitoredResource(res))
    ```
*   `slogcp.WithLevel(slog.Level)`: Sets the minimum log level, overriding `LOG_LEVEL`. Use standard `slog.Level` constants or `slogcp.Level` constants cast to `slog.Level` (e.g., `slog.Level(slogcp.LevelNotice)`).
    ```go
    logger, err := slogcp.New(slogcp.WithLevel(slog.LevelDebug))
    ```
*   `slogcp.WithSourceLocationEnabled(bool)`: Enables or disables source code location logging, overriding `LOG_SOURCE_LOCATION`. Defaults to `false`.
    ```go
    logger, err := slogcp.New(slogcp.WithSourceLocationEnabled(true))
    ```
*   `slogcp.WithStackTraceEnabled(bool)`: Enables or disables stack trace capture, overriding `LOG_STACK_TRACE_ENABLED`. Defaults to `false`.
    ```go
    logger, err := slogcp.New(slogcp.WithStackTraceEnabled(true))
    ```
*   `slogcp.WithStackTraceLevel(slog.Level)`: Sets the minimum level for stack trace capture when enabled, overriding `LOG_STACK_TRACE_LEVEL`. Defaults to `slog.LevelError`.
    ```go
    // Capture stack traces for WARN level and above
    logger, err := slogcp.New(
        slogcp.WithStackTraceEnabled(true),
        slogcp.WithStackTraceLevel(slog.LevelWarn),
    )
    ```
*   `slogcp.WithAttrs([]slog.Attr)`: Adds attributes to every log entry produced by this logger instance and its descendants. Equivalent to calling `logger.With(...)` immediately after creation. Can be used multiple times; attributes accumulate.
    ```go
    attrs := []slog.Attr{
        slog.String("service.region", "us-central1"),
        slog.String("service.instance_id", "abc-123"),
    }
    logger, err := slogcp.New(slogcp.WithAttrs(attrs))
    ```
*   `slogcp.WithGroup(string)`: Adds a group namespace to the logger. Equivalent to calling `logger.WithGroup(...)` immediately after creation (and after any `WithAttrs`). If used multiple times, only the last one applies.
    ```go
    logger, err := slogcp.New(slogcp.WithGroup("request_details"))
    // Logs will have attributes nested under "request_details":
    // { "request_details": { "user_id": "..." } }
    ```
*   `slogcp.WithEntryCountThreshold(int)`: Sets the maximum number of log entries to buffer before sending them to the Cloud Logging API. Overrides the default used by the underlying client library. See [logging.EntryCountThreshold](https://pkg.go.dev/cloud.google.com/go/logging#EntryCountThreshold).
    ```go
    logger, err := slogcp.New(slogcp.WithEntryCountThreshold(500)) // Buffer up to 500 entries
    ```
*   `slogcp.WithDelayThreshold(time.Duration)`: Sets the maximum time log entries are buffered before sending. Overrides the default used by the underlying client library. See [logging.DelayThreshold](https://pkg.go.dev/cloud.google.com/go/logging#DelayThreshold).
    ```go
    import "time"
    // ...
    logger, err := slogcp.New(slogcp.WithDelayThreshold(5*time.Second)) // Send logs at least every 5 seconds
    ```
*   `slogcp.WithCloudRunPayloadAttributes(bool)`: Enables or disables adding Cloud Run service (`K_SERVICE`) and revision (`K_REVISION`) names as attributes within the log payload, if detected in the environment. This is distinct from the MonitoredResource association. Defaults to `false`.
    ```go
    logger, err := slogcp.New(slogcp.WithCloudRunPayloadAttributes(true))
    ```

## Why Use slogcp?

### What does the ideal logging situation on Google Cloud Run look like?

An optimal logging setup on Cloud Run provides clear, actionable insights into application behavior with minimal friction. Logs are not just captured; they actively support debugging, monitoring, and tracing by integrating seamlessly with Google Cloud's observability tools. This means logs are structured, context-rich, reliable, and performant.

#### Structured Logging
The cornerstone of effective logging on Google Cloud Run is **structured logging**. Log entries formatted as JSON objects allow Google Cloud Logging to automatically parse, index, and correlate information. This machine-readable format is fundamental for enabling advanced filtering, searching based on specific fields, creating logs-based metrics, setting up alerts, and integrating seamlessly with other observability tools like Cloud Trace and Cloud Error Reporting. Unstructured text logs lack this capability, limiting analysis and automation.

#### Direct API Integration

While Cloud Run conveniently captures standard output (stdout/stderr), applications needing higher volume throughput or greater reliability guarantees (e.g., minimizing log loss during abrupt instance termination) directly use the Cloud Logging API via client libraries.

#### Rich Contextual Information

Log entries automatically include relevant context without manual effort at every log site. This means:
*   **Trace Correlation:** Logs automatically correlate with Cloud Trace requests using standard fields like `logging.googleapis.com/trace` and `logging.googleapis.com/spanId`. This links application behavior directly to specific requests.
*   **HTTP Request Data:** Relevant details from the originating HTTP request (method, URL, status, latency, user agent) are associated with logs, often via the `httpRequest` field, providing immediate request context.
*   **Source Location:** Accurate source code location (`logging.googleapis.com/sourceLocation` including file, line, and function) is included, linking logs directly back to the code that generated them.
*   **Custom Attributes:** Adding application-specific structured attributes (e.g., user IDs, business transaction details) is straightforward and integrates naturally with the structured payload.

#### Accurate Severity Levels

Logs accurately reflect their importance using the full range of Google Cloud Logging severity levels (DEBUG, INFO, NOTICE, WARNING, ERROR, CRITICAL, ALERT, EMERGENCY). This allows developers to **filter logs precisely in the Logs Explorer and configure alerts based on specific severities** (e.g., triggering an alert only for ERROR or CRITICAL messages).

#### Seamless Error Reporting

Errors, especially those logged with stack traces formatted according to platform expectations (e.g., in a `stack_trace` field), are automatically parsed and grouped by Cloud Error Reporting. This helps **find and fix errors faster** by grouping similar issues and providing context without requiring separate error reporting setup.

#### Performance and Reliability

Logging is designed for low performance overhead. When using the Cloud Logging API directly, efficient asynchronous batching sends logs reliably without blocking application threads. Graceful shutdown procedures ensure buffered logs are flushed before instance termination, minimizing data loss.

#### Developer-Friendly Configuration

Configuration is straightforward and environment-aware. The logging system automatically adapts its output format (e.g., human-readable console logs locally vs. GCP-compliant JSON in Cloud Run). Features like source location or stack trace inclusion are easily configurable (and ideally off by default in production for performance), and log levels can often be adjusted dynamically at runtime for targeted debugging without redeployments.

Okay, here is that subsection, detailing the practical difficulties developers face when trying to achieve the ideal Cloud Run logging setup using standard Go tools like log/slog, based on the provided research documents.

### So why doesn't everyone just set up their logging that way?

Because it's a huge pain in the ass!

Achieving that ideal logging state on Google Cloud Run using Go's standard log/slog package requires navigating a significant amount of configuration complexity and platform-specific nuances. Common developer painpoints include:

#### Getting the Basic GCP JSON Format Right

Simply using `slog.NewJSONHandler` isn't enough. Google Cloud Logging expects specific field names and value formats that differ from slog's defaults. Developers must manually configure the handler, typically using `HandlerOptions.ReplaceAttr`, to:
*   **Rename Keys:** Change slog's default `level` key to `severity`, `msg` to `message`, and `source` to `logging.googleapis.com/sourceLocation`.
*   **Map Severity Values:** Convert slog's integer `Level` constants (e.g., `slog.LevelInfo`, `slog.LevelError`) into the specific uppercase string values Cloud Logging recognizes ("INFO", "ERROR", "WARNING", etc.). This requires custom mapping logic within the `ReplaceAttr` function.
*   **Format Source Location:** If source location is enabled (`AddSource: true`), the default `*slog.Source` value needs to be transformed into the nested JSON object `{"file": "...", "line": "...", "function": "..."}` expected by the `logging.googleapis.com/sourceLocation` field.

This requires writing non-trivial, GCP-specific boilerplate code just to get the basic field structure correct.

#### Implementing Trace Correlation

Automatically linking application logs to Cloud Run request logs and Cloud Trace spans is crucial but complex. There's no built-in slog mechanism for this. Developers must:
1.  **Write HTTP Middleware:** To intercept incoming requests and read the `X-Cloud-Trace-Context` header provided by GCP.
2.  **Parse the Header:** Extract the `TRACE_ID` portion from the header string.
3.  **Get Project ID:** Reliably determine the Google Cloud Project ID, often by querying the metadata server or reading environment variables.
4.  **Format the Trace String:** Construct the specific `projects/PROJECT_ID/traces/TRACE_ID` string.
5.  **Propagate via Context:** Inject this formatted trace string into the request's `context.Context`.
6.  **Implement a Custom Handler:** Write a custom `slog.Handler` wrapper. This wrapper's `Handle` method needs to retrieve the trace string from the context passed to logging functions (like `slog.InfoContext`) and add the `logging.googleapis.com/trace` attribute (and potentially `spanId` and `trace_sampled`) to the log record before passing it to the underlying JSON handler.
7.  **Handle `WithAttrs`/`WithGroup` Correctly:** The custom handler wrapper must correctly implement the `WithAttrs` and `WithGroup` methods to ensure trace context isn't lost when derived loggers are created using `logger.With(...)`. This is a common and subtle pitfall.

This multi-step process involving middleware, context management, and careful custom handler implementation is far from trivial and needs to be done for every service.

#### Logging Stack Traces for Error Reporting

Getting errors automatically grouped by Cloud Error Reporting requires logging stack traces in a specific way. However:
*   **slog Doesn't Capture Stacks:** The standard `slog` package doesn't automatically capture stack traces when logging an `error` value.
*   **Go Errors Lack Standard Stacks:** Standard Go `error` values don't inherently contain stack traces.
*   **Manual Work Required:** Developers must either:
    *   Use error-wrapping libraries (like the archived `pkg/errors` or modern alternatives) that capture stacks, then write custom handler logic to detect these error types, extract the trace, and format it.
    *   Manually capture a stack trace at log time using `runtime.Stack()` within the handler, which might not reflect the error's origin and incurs performance costs.
*   **GCP Formatting:** The extracted stack trace needs to be formatted as a single string (matching `runtime.Stack` output) and included under the specific key `stack_trace` in the JSON payload, typically only for logs with `severity` ERROR or higher.

This requires choosing an error handling strategy, integrating it with slog, and implementing custom formatting logic within the handler, often conditionally based on log level.

#### Managing Performance Overhead

Features essential for debugging can impact performance:
*   **Source Location (`AddSource`):** Enabling source location in `slog` incurs overhead because `runtime.Caller` is invoked *before* the log level check, meaning the cost is paid even for logs that are ultimately discarded. This makes developers hesitant to enable it by default in production.
*   **Stack Traces:** Capturing stack traces, especially using `runtime.Stack`, is computationally expensive and significantly increases log size.

Optimizing logging often means making these features configurable (e.g., enabling source/stacks only for ERROR levels or via runtime flags), adding further complexity to the handler logic.

#### Environment-Specific Formatting (Dev vs. Prod)

Developers need readable console logs locally but structured JSON in Cloud Run. Standard `slog` handlers don't automatically switch:
*   `slog.TextHandler` is often considered too verbose or cluttered for easy local debugging.
*   `slog.JSONHandler` is unreadable in a local console.
Achieving automatic switching requires:
1.  Detecting the environment (e.g., checking for the `K_SERVICE` environment variable in Cloud Run).
2.  Conditionally initializing different `slog.Handler` instances (e.g., a human-friendly console handler like `tint` locally, versus the custom GCP JSON handler in Cloud Run).

This adds initialization complexity and often involves third-party handlers for a better local developer experience.

#### Custom Handler Complexity

While `ReplaceAttr` handles basic field mapping, integrating trace context and stack traces almost always requires implementing a custom `slog.Handler`. As mentioned, correctly implementing all four methods (`Enabled`, `Handle`, `WithAttrs`, `WithGroup`) of the interface, especially ensuring `WithAttrs` and `WithGroup` preserve the wrapper's behavior, is complex and error-prone.
