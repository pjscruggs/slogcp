# Using slogcp in a Go application

Use `slogcp` as the handler behind the standard library's `log/slog` API. Keep
logging through `*slog.Logger` and choose global, injected, or request-scoped
loggers to match your application. Add HTTP, gRPC, or Pub/Sub helpers only where
you need their integration behavior.

This guide covers application wiring and logging patterns. See the
[README](../README.md) for dependency comparisons, the [configuration
reference](CONFIGURATION.md) for options and environment variables, and the
[package documentation](https://pkg.go.dev/github.com/pjscruggs/slogcp) for API
contracts.

**In this guide:** [Get started](#get-started) · [Choose a
logger](#choose-a-logger) · [Add integrations](#add-integrations) · [Correlate
logs with traces](#correlate-logs-with-traces) · [Log errors](#log-errors) ·
[Manage configuration and shutdown](#manage-configuration-and-shutdown) ·
[Verify the integration](#verify-the-integration)

## Get started

Check your application's supported toolchain against the minimum Go version
declared in [`go.mod`](../go.mod) for the slogcp release you use. A dependency's
[`toolchain` directive](https://go.dev/doc/toolchain) is not an additional
minimum imposed on its importers.

From your application's existing Go module, add slogcp:

```sh
go get github.com/pjscruggs/slogcp/v2
```

No checkout of the slogcp repository is required. The handler writes JSON to an
`io.Writer`. It does not send entries to the Cloud Logging API. Cloud Run
[collects stdout and stderr automatically][cloud-run-logging]. For other
environments, make sure your deployment collects the chosen stream or file.
Producing Cloud-compatible JSON and delivering it to Cloud Logging are separate
responsibilities.

### Create the handler at application startup

This complete program creates a logger without changing the global default.

```go
package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/pjscruggs/slogcp/v2"
)

func main() {
	handler, err := slogcp.NewHandler(os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "configure logging:", err)
		os.Exit(1)
	}
	defer func() {
		if err := handler.Close(); err != nil {
			fmt.Fprintln(os.Stderr, "close logging:", err)
		}
	}()

	logger := slog.New(handler).With("component", "orders")
	logger.Info("application started")
}
```

With default configuration, the program writes a JSON entry containing the
message and `component` attribute. Severity spelling and timestamp emission
depend on runtime detection and configuration. Keep application fields
structured instead of JSON-encoding an object and logging that JSON as the
message.

`os.Stdout` is the default writer for this constructor call. The environment
variable `SLOGCP_TARGET` can select `stdout`, `stderr`, or `file:<path>`. Set it
before starting the application, as shown in the [configuration
examples](#manage-configuration-and-shutdown). You can also pass redirect
options to `NewHandler`. Construction reads the environment and applies
programmatic options. Always check the returned error.

The deferred close accommodates file redirection or explicit buffering. Default
synchronous stdout/stderr logging does not require flushing an internal queue.
For a long-running service, keep the handler alive until application shutdown,
not merely until the initialization function returns.

The remaining snippets are application-code excerpts. Unless a snippet supplies
its own setup, `logger` is a configured `*slog.Logger`, `handler` is its owning
`*slogcp.Handler`, and `ctx` is the current operation's `context.Context`.

## Choose a logger

**Selecting a logger and passing a context are different operations.** The
[`slog` context-taking methods](https://pkg.go.dev/log/slog#hdr-Contexts) pass
context to the selected handler. They do not look up a logger stored in it.

| Call | Logger selected | Context delivered to the handler |
| --- | --- | --- |
| `slog.InfoContext(ctx, ...)` | The current process-wide default | `ctx` |
| `logger.InfoContext(ctx, ...)` | The explicit `logger` | `ctx` |
| `slogcp.Logger(ctx).InfoContext(ctx, ...)` | The logger stored in `ctx`, otherwise the current default | `ctx` |
| `slogcp.Logger(ctx).Info(...)` | The logger stored in `ctx`, otherwise the current default | No caller-supplied context |

A logger derived by middleware may already have request or trace attributes
bound to it. Passing the current context lets the handler inspect the active
span at the logging call, including a child span created after the request
logger was derived.

### Keep a global logging pattern

Set the default during application startup, before constructing integrations
that capture it:

```go
slog.SetDefault(logger)
slog.InfoContext(ctx, "worker started")
```

Use this when the application already relies on top-level slog calls. It is not
required for injected loggers. Changing the default does not replace logger
pointers already stored in components or middleware. In request handlers,
top-level slog calls also do not select the middleware's request-scoped logger.
See [`slog.SetDefault`](https://pkg.go.dev/log/slog#SetDefault).

### Keep dependency-injected loggers

Pass `*slog.Logger` into the components that need it. Components do not need to
accept `*slogcp.Handler` or adopt a slogcp-specific logging interface:

```go
type Worker struct {
	logger *slog.Logger
}

func NewWorker(logger *slog.Logger) *Worker {
	return &Worker{logger: logger.With("component", "worker")}
}

func (w *Worker) Process(ctx context.Context, jobID string) {
	w.logger.InfoContext(ctx, "processing job", "job_id", jobID)
}
```

This excerpt uses `context` and `log/slog`. Supply a non-nil logger at
construction. The injected logger retains its own attributes and handler
pipeline. Receiving a request context does not make it inherit attributes from
another logger stored in that context.

### Use a request-scoped logger, with an explicit fallback when needed

Inside slogcp HTTP middleware, native gRPC interceptors, or the Pub/Sub receive
wrapper, select the logger they placed in the operation's context:

```go
slogcp.Logger(ctx).InfoContext(ctx, "processing request")
```

Use `LoggerFromContext` when absence should fall back to an injected logger
rather than to `slog.Default()`. The following application helper uses
`context`, `log/slog`, and `github.com/pjscruggs/slogcp/v2`:

```go
func loggerFor(ctx context.Context, fallback *slog.Logger) *slog.Logger {
	if contextual, ok := slogcp.LoggerFromContext(ctx); ok {
		return contextual
	}
	return fallback
}
```

Call `loggerFor(ctx, logger).InfoContext(ctx, ...)` with a non-nil fallback.
`LoggerFromContext` returns a presence flag without consulting the global
logger. See the [context helper implementation](../context.go).

To enrich the current operation further, derive from the selected logger and
pass on the returned context:

```go
requestLogger := slogcp.Logger(ctx).With("order_id", "order-42")
ctx = slogcp.ContextWithLogger(ctx, requestLogger)
slogcp.Logger(ctx).InfoContext(ctx, "order accepted")
```

A child context does not modify its parent. An outer middleware or interceptor
that retained the earlier context will not automatically see this later logger.
Derive related loggers from a common base when they should share attributes,
filtering, redaction, and output destinations. Selecting a contextual logger
does not merge two independent logger pipelines.

### Bind attributes without changing their intended placement

Keep the logger returned by `logger.With(...)`. The original logger is
unchanged. For an application-owned nested field, use an explicit group:

```go
logger.InfoContext(ctx, "order accepted",
	slog.Group("order", slog.String("id", "order-42")),
)
```

`WithGroup` instead groups subsequent attributes, including attributes that
integrations attach to that derived logger. Pass an ungrouped base into
middleware when you expect its request fields at their documented locations. See
[`slog.Logger.WithGroup`](https://pkg.go.dev/log/slog#Logger.WithGroup).

## Add integrations

Start with the core handler. Choose additional packages according to the
behavior the application needs, not simply the protocols it uses.

| Application need | Integration |
| --- | --- |
| Cloud-compatible JSON from existing slog calls and instrumentation | `github.com/pjscruggs/slogcp/v2` alone |
| HTTP-scoped application logging and propagation helpers | `github.com/pjscruggs/slogcp/v2/slogcphttp` |
| gRPC-scoped application logging, RPC information, and optional OTel wiring | `github.com/pjscruggs/slogcp/v2/slogcpgrpc` |
| go-grpc-middleware logging events routed through a slog logger | Separate module `github.com/pjscruggs/slogcp-grpc-adapter/v2` |
| Pub/Sub message-context propagation and scoped receive-handler logging | `github.com/pjscruggs/slogcp-pubsub` |

The native HTTP, gRPC, and Pub/Sub helpers enrich application logs. They do not
emit access or message-receive logs by themselves. The adapter supplies the
logger used by upstream gRPC logging interceptors that emit RPC events.

### HTTP application logging

**Cloud Run services already generate HTTP request logs automatically.** In that
environment, application code generally does not need to produce a second access
record for each request. Prefer the platform’s request logs for HTTP access
logging, and use application logs for business events, errors, and diagnostic
details, correlated with the request through trace context. ([Google Cloud
Documentation][http-cloud-run-logging])

Add `net/http` and `github.com/pjscruggs/slogcp/v2/slogcphttp` to your imports.
This excerpt builds a handler to attach to your existing HTTP server:

```go
mux := http.NewServeMux()
mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	slogcp.Logger(ctx).InfoContext(ctx, "health check")
	w.WriteHeader(http.StatusNoContent)
})

wrapped := slogcphttp.Middleware(
	slogcphttp.WithLogger(logger),
	slogcphttp.WithPropagators(slogcp.NewCompositePropagator()),
)(mux)
```

Use `wrapped` as the server’s `Handler`. The middleware derives a request-scoped
logger and attaches it to the request context. Retrieve that logger with
`slogcp.Logger(ctx)` to include its bound request attributes; calling the
original `logger.InfoContext(ctx, ...)` passes the context but does not select
the middleware’s logger.

By default, `Middleware` wraps the application handler with `otelhttp`
instrumentation. For an application that already has the appropriate outer
`otelhttp` wrapper, retain it and set `slogcphttp.WithOTel(false)` on the inner
slogcp middleware. Keep the existing instrumentation outside the logging
middleware so the latter receives its request context. Disabling slogcp’s
instrumentation wrapper does not itself disable slogcp’s trace-context
propagation.

For that existing-instrumentation setup, replace the `wrapped` construction
above with the following. Add
`go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp` to your imports
and preserve your application's existing OTel handler options:

```go
loggingHandler := slogcphttp.Middleware(
	slogcphttp.WithLogger(logger),
	slogcphttp.WithOTel(false),
)(mux)
wrapped := otelhttp.NewHandler(loggingHandler, "http.server",
	otelhttp.WithPropagators(slogcp.NewCompositePropagator()),
)
```

Use `wrapped` as the server's `Handler`. With a recording tracer provider
configured by your application, the outer wrapper creates the server span before
slogcp derives the request logger. Application logs then use that span's
context, and slogcp does not create a second instrumentation wrapper.

#### Optional HTTP request records

When your application has a specific need for its own request records—or runs in
an environment without platform-generated access logs—slogcp provides helpers
that format HTTP metadata according to Cloud Logging’s `httpRequest` structure.
You do not need to construct that payload manually. To attach it to an
individual application log entry, use the following inside a request handler:
([Google Cloud Documentation][http-log-entry])

```go
ctx := r.Context()
slogcp.Logger(ctx).InfoContext(ctx, "request details",
	slogcphttp.HTTPRequestAttrFromContext(ctx, r),
)
```

Alternatively, add `slogcphttp.WithHTTPRequestAttr(true)` to the middleware
options to include the payload on entries written through the derived request
logger. **This enables request-metadata formatting, not automatic access
logging:** your application still decides whether and when to emit a record.

A record emitted while the request is in progress is not a completion record.
The helpers omit final response status, response size, and latency until the
middleware’s request scope is finalized. For a custom completion record, emit
after finalization; `slogcphttp.HTTPRequestFromScope` can snapshot the completed
scope into the same Cloud Logging-compatible structure.

See the HTTP guide in `slogcphttp/README.md` for request fields, route
extraction, client transport, and HTTP request payload helpers.

### Native gRPC integration

Add `google.golang.org/grpc` and `github.com/pjscruggs/slogcp/v2/slogcpgrpc` to
your imports. Construct the server with the native option bundle:

```go
server := grpc.NewServer(slogcpgrpc.ServerOptions(
	slogcpgrpc.WithLogger(logger),
	slogcpgrpc.WithPropagators(slogcp.NewCompositePropagator()),
)...)
```

Register and serve your application services as usual. Inside a unary handler,
use `slogcp.Logger(ctx).InfoContext(ctx, ...)`. Inside a streaming handler, use
the wrapped stream's `Context()`.

`ServerOptions` and `DialOptions` install OTel stats handlers when enabled. The
individual `UnaryServerInterceptor`, `StreamServerInterceptor`, and client
interceptor constructors do not install those stats handlers. For an application
that already owns the corresponding OTel stats handler, use
`slogcpgrpc.WithOTel(false)` with the option bundle rather than installing the
same instrumentation again. Provider and exporter setup remain application
concerns. See the [native gRPC guide](../slogcpgrpc/README.md) and
[option-bundle implementation](../slogcpgrpc/interceptors.go).

### Optional integration with go-grpc-middleware

`slogcp-grpc-adapter` is an **optional compatibility layer between slogcp and
`grpc-ecosystem/go-grpc-middleware`**. It lets applications use that ecosystem’s
logging interceptors with a slogcp-backed logger, while keeping their existing
middleware chains and application-level `log/slog` API. The adapter lives in a
separate Go module so adopting slogcp does not require adopting
go-grpc-middleware.

The integration connects two responsibilities: go-grpc-middleware determines
which RPC events to log and supplies their fields and levels; the adapter
implements its `logging.Logger` interface and forwards those events through your
`*slog.Logger`. With a slogcp handler, they receive the same Google
Cloud-compatible formatting and configured logging behavior as your application
logs. This works with unary and streaming RPCs on both servers and clients.

To add the integration, install the optional module:

```sh
go get github.com/pjscruggs/slogcp-grpc-adapter/v2
```

Add these imports to your server setup:

```go
import (
	grpc_logging "github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/logging"
	slogcpadapter "github.com/pjscruggs/slogcp-grpc-adapter/v2"
	"google.golang.org/grpc"
)
```

Using an existing slogcp-backed `logger`, construct the adapter and pass it to
the upstream logging interceptors:

```go
adapted := slogcpadapter.NewLogger(nil,
	slogcpadapter.WithLogger(logger),
)

server := grpc.NewServer(
	grpc.ChainUnaryInterceptor(
		grpc_logging.UnaryServerInterceptor(adapted),
	),
	grpc.ChainStreamInterceptor(
		grpc_logging.StreamServerInterceptor(adapted),
	),
)
```

In an existing server, use `adapted` in your existing logging interceptor
constructors rather than adding another logging interceptor. `WithLogger`
preserves the supplied logger’s bound attributes and handler pipeline. Continue
configuring RPC logging through upstream options such as
`grpc_logging.WithFieldsFromContext`, `grpc_logging.WithLevels`, and
`grpc_logging.WithLogOnEvents`; the adapter changes where those events are
written, not which middleware options are available.

Logger selection is fixed by default. To use request-scoped loggers stored with
`slogcp.ContextWithLogger`, opt in when constructing the adapter:

```go
adapted := slogcpadapter.NewLogger(nil,
	slogcpadapter.WithLogger(logger),
	slogcpadapter.WithLoggerPolicy(slogcpadapter.PreferContext),
)
```

This selects the contextual logger when present and otherwise uses the
construction-time logger. It selects the whole logger, including its attributes,
filtering, and destination; it does not merge separate loggers. Install the
contextual logger before the upstream logging interceptor runs. The adapter’s
convenience interceptor helpers retain fixed selection.

**Native `slogcpgrpc` support remains available independently of this
integration.** It derives RPC-scoped loggers and enriches application logs, but
does not emit access logs itself. You can use it alongside the adapter when you
need both native enrichment and go-grpc-middleware’s RPC logging events: place
native enrichment earlier in the interceptor chain and enable `PreferContext` so
those events use the enriched logger.

The adapter forwards each logging call’s context so slogcp can correlate the
event with an available trace. Trace instrumentation and export remain separate
responsibilities of your application’s OpenTelemetry setup.

### Pub/Sub and other client libraries

The optional [`slogcp-pubsub`](https://github.com/pjscruggs/slogcp-pubsub)
module provides trace propagation through Pub/Sub message
attributes and message-scoped application logging. Use the publishing helpers
before sending a message, and choose the receiving integration according to
whether your application receives messages through a Go callback or an HTTP
endpoint.

**Publishing.** Call `slogcppubsub.Inject(ctx, msg)` before publishing, using
the context of the operation producing the message. This writes trace context
into `msg.Attributes` so a consumer can recover it when processing the message.

**Pull subscribers.** Wrap the callback passed to the Go Pub/Sub client’s
`Receive` method with `slogcppubsub.WrapReceiveHandler`, supplying
`slogcppubsub.WithLogger(logger)` to derive message loggers from your configured
application logger. Inside the callback, use:

```go
slogcp.Logger(ctx).InfoContext(ctx, "processing message")
```

The wrapper extracts trace context from the message attributes, optionally
starts a consumer span, and attaches a message-scoped logger to the callback
context. Your callback remains responsible for processing the message, emitting
application logs, and acknowledging or negatively acknowledging delivery; the
wrapper does not do those things automatically.

For example, this callback acknowledges successful processing and requests
redelivery on failure. Add `context`, `log/slog`,
`cloud.google.com/go/pubsub/v2`, and `github.com/pjscruggs/slogcp-pubsub`
to your imports. Supply your application's `process` function with signature
`func(context.Context, []byte) error`:

```go
receive := slogcppubsub.WrapReceiveHandler(
	func(ctx context.Context, msg *pubsub.Message) {
		messageLogger := slogcp.Logger(ctx)
		if err := process(ctx, msg.Data); err != nil {
			messageLogger.ErrorContext(ctx, "process message failed",
				slog.Any("error", err),
			)
			msg.Nack()
			return
		}
		messageLogger.InfoContext(ctx, "message processed")
		msg.Ack()
	},
	slogcppubsub.WithLogger(logger),
	slogcppubsub.WithPropagators(slogcp.NewCompositePropagator()),
)
```

Pass `receive` to your existing subscriber's `Receive(ctx, receive)` call and
handle its returned error. The callback passes the extracted message context
into both processing and logging. Adapt the failure policy to your application;
processing must tolerate redelivery, and `Receive` can invoke callbacks
concurrently.

**HTTP push subscribers.** Use `slogcphttp` to wrap the receiving HTTP endpoint.
`WrapReceiveHandler` wraps a Go receive callback, not an HTTP handler. With
Pub/Sub’s default wrapped delivery, message attributes are inside the JSON
request body as `message.attributes`. To recover producer trace context from
those attributes, decode the envelope and pass the attributes to
`slogcppubsub.ExtractAttributes`; use the returned context for
message-processing logs and instrumentation. Reading HTTP headers alone does not
recover trace context stored inside the JSON body. ([Google Cloud
Documentation][pubsub-push])

For an unwrapped push subscription, enable **Write metadata** to deliver message
attributes as HTTP headers. When the publisher supplies standard W3C
`traceparent` and `tracestate` attributes, your configured HTTP trace propagator
can then extract them from those headers. The Pub/Sub guide in
[`slogcp-pubsub`](https://github.com/pjscruggs/slogcp-pubsub) covers propagation options, optional consumer spans,
and interoperability with the Go client’s trace attributes. ([Google Cloud
Documentation][pubsub-unwrapping])

**Other client libraries.** For a library that accepts a `*slog.Logger`, pass
your configured logger through its documented logger option—for example,
`option.WithLogger(logger)` in Google Cloud clients that support it. Reuse that
logger, or derive a child with `logger.With(...)`, to retain its handler
pipeline and bound attributes. When adopting slogcp as the output handler,
preserve any existing redaction, filtering, and other handler wrappers rather
than replacing the entire logging setup with an unrelated logger. ([Google Cloud
Documentation][go-client-observability])

## Correlate logs with traces

Log correlation, propagation, and trace export each need their own
configuration.

For **log correlation**, the slogcp handler reads the span context passed to a
log call. A resolved project ID lets it write the fully qualified Cloud trace
field `projects/PROJECT_ID/traces/TRACE_ID`. Runtime detection supplies the
project where available. To choose explicitly, pass
`slogcp.WithTraceProjectID("example-project")` to `NewHandler` or set the
`SLOGCP_TRACE_PROJECT_ID` environment variable to your trace project's ID before
startup. The [configuration examples](#manage-configuration-and-shutdown) show
how to set it. Pass the current operation context to each log call. A bare
logger call cannot discover the current request's span by itself.

For **propagation**, HTTP/gRPC/message integrations extract and inject context
at transport boundaries. `slogcp.NewCompositePropagator()` accepts legacy
`X-Cloud-Trace-Context` on ingress and uses W3C Trace Context and baggage for
outgoing propagation. It does not change global OTel configuration.
`EnsurePropagation()` installs that composite globally once, replacing your
existing propagator. Preserve an application's established propagator
configuration unless changing it is intentional. See
[`propagation.go`](../propagation.go).

Installing middleware or adding IDs to a log entry does not configure **trace
recording or export**. Keep or initialize the application's OTel providers and
export pipeline separately. No recording/export pipeline is needed merely to
correlate a log with an already valid trace context. See [OpenTelemetry Go
instrumentation][otel-instrumentation] and
[exporters](https://opentelemetry.io/docs/languages/go/exporters/).

The handler can correlate to a remote trace without claiming its span as a local
span. Its automatic `logging.googleapis.com/spanId` field is emitted for a
valid, non-remote span context. A request with only extracted remote context can
therefore have a trace field without a locally generated span ID. Creating a
correlation field also does not prove that a matching trace has been exported or
retained. See the [handler implementation](../json_handler.go).

## Log errors

Keep errors as error values so slogcp can inspect their type and supported stack
information:

```go
logger.ErrorContext(ctx, "create order failed", slog.Any("error", err))
```

Use this when `err` is non-nil. Converting it to `err.Error()` before logging
loses the error value that enables this processing. Automatic stack capture is
disabled by default. Enable `WithStackTraceEnabled(true)` at handler
construction when that is your chosen policy. Setting only `WithStackTraceLevel`
does not enable it. A carried stack can still be recognized without automatic
capture.

Use `ReportError` to attach Error Reporting metadata.

```go
slogcp.ReportError(ctx, logger, err, "create order failed")
```

For a non-nil error, `ReportError` attaches stack/report-location attributes and
available service metadata, and logs through the supplied logger. It is not a
separate Error Reporting API client, and the logger's level filtering still
applies. When runtime service metadata is unavailable, pass
`slogcp.WithErrorServiceContext(service, version)` as an option to `ReportError`
or `ErrorReportingAttrs`, using your application's deployment identity. Keep
this logger ungrouped when you need the helper's attributes at the Cloud Logging
field locations.

Choose either a helper or ordinary error logging for each event to avoid logging
the same failure twice. See the [error helper
API](https://pkg.go.dev/github.com/pjscruggs/slogcp#ReportError) and [Google's
Error Reporting log requirements][error-reporting-format].

## Manage configuration and shutdown

Set slogcp's environment variables in the shell that launches your application
or in your deployment's container or service environment. For example, these
commands select stdout and an explicit trace project for a local application.
Replace `example-project` with the ID of the project that owns your traces.

In a POSIX shell such as Bash or Zsh, export the variables before starting Go.

```sh
export SLOGCP_TARGET=stdout
export SLOGCP_TRACE_PROJECT_ID=example-project
go run .
```

In PowerShell, assign them through the environment provider.

```powershell
$env:SLOGCP_TARGET = "stdout"
$env:SLOGCP_TRACE_PROJECT_ID = "example-project"
go run .
```

These settings apply to the current shell and the processes it starts. In a
deployment, configure the same names and values as environment variables before
the application starts. The `With...` functions shown elsewhere in this guide
are Go options passed to constructors or helpers.

Use deployment configuration for values that vary by environment and options for
application-owned policy. Handler options override corresponding environment
values after environment parsing succeeds. An invalid value of the
`SLOGCP_TARGET` environment variable (including `file:` without a path) makes
construction fail with `ErrInvalidRedirectTarget` before redirect options are
applied. Fix or unset that environment value even when supplying
`WithRedirectWriter`.

Environment values are read at construction, not watched continuously. Use
`handler.SetLevel(...)` or a shared `slog.LevelVar` for live level changes. The
[configuration reference](CONFIGURATION.md) is the complete option reference.

For custom attribute redaction, `WithReplaceAttr` applies to user attributes,
including bound and grouped attributes. Automatically generated fields are added
after that hook, so it cannot filter the complete output. Preserve any broader
redaction policy in your existing handler composition rather than assuming this
hook covers every generated field.

Stop accepting work and let logging producers finish before closing the shared
handler. Close once at the owning application's shutdown boundary, not once per
request or derived logger. File targets use buffering by default. Explicitly
configured async handlers also need their shutdown path to run. `os.Exit` and
`log.Fatal` do not run deferred cleanup, so return from the function that owns
the handler before terminating the process.

For a caller-owned file or rotation writer, use `WithRedirectWriter(writer)` to
keep ownership explicit. After the logging handler has finished, flush or close
that writer according to its own API. See the configuration reference for file
rotation, asynchronous logging, and shutdown options.

Internal slogcp diagnostics use a discarding logger by default. To investigate
configuration, encoding, or write problems, pass an independent diagnostic
logger when constructing the handler. This excerpt uses `os` and `log/slog`:

```go
diagnostics := slog.New(slog.NewTextHandler(os.Stderr, nil))
handler, err := slogcp.NewHandler(os.Stdout,
	slogcp.WithInternalLogger(diagnostics),
)
```

Handle `err` and retain the normal shutdown path. Do not route this diagnostic
logger back through the same slogcp handler.

## Verify the integration

### Check the application locally

After adding the actual imports, run from your application module:

```sh
go mod tidy
go test ./...
```

Check behavior as well as compilation. This test checks JSON output, severity,
request-logger selection, and trace-field formatting without a Cloud account.
Put it in your application's test suite and adjust the package name as needed:

```go
package logging_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/pjscruggs/slogcp/v2"
	"go.opentelemetry.io/otel/trace"
)

func TestSlogcpRequestLog(t *testing.T) {
	// Set a valid environment variable so parsing succeeds before
	// WithRedirectWriter overrides the output destination.
	t.Setenv("SLOGCP_TARGET", "stdout")

	var out bytes.Buffer
	handler, err := slogcp.NewHandler(&out,
		slogcp.WithRedirectWriter(&out),
		slogcp.WithLevel(slog.LevelInfo),
		slogcp.WithTraceProjectID("example-project"),
		slogcp.WithSeverityAliases(false),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := handler.Close(); err != nil {
			t.Error(err)
		}
	})

	// A synthetic local span context checks formatting, not trace export.
	spanContext := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{1},
		SpanID:     trace.SpanID{2},
		TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), spanContext)
	base := slog.New(handler)
	requestLogger := base.With("request_id", "request-1")
	ctx = slogcp.ContextWithLogger(ctx, requestLogger)

	slogcp.Logger(ctx).InfoContext(ctx, "verification")

	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("invalid log JSON (%v), output %s", err, out.String())
	}
	wantTrace := "projects/example-project/traces/" +
		spanContext.TraceID().String()
	wantSpan := spanContext.SpanID().String()
	want := map[string]any{
		"severity":                             "INFO",
		"message":                              "verification",
		"request_id":                           "request-1",
		"logging.googleapis.com/trace":         wantTrace,
		"logging.googleapis.com/spanId":        wantSpan,
		"logging.googleapis.com/trace_sampled": true,
	}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("%s: got %v, want %v", key, got[key], value)
		}
	}
}
```

A passing test does not verify transport propagation, middleware ordering, Cloud
ingestion, or trace export. Add an HTTP/RPC test that exercises the
application's real chain and checks its request fields. Test filtering or
redaction through the same configured logger pipeline used by the application.

### Check the deployed result

Generate an identifiable application log through a real request or job. In
[Cloud Run's log views or Logs Explorer][cloud-run-logging], check the parsed
severity, application attributes, and the trace field when the operation has
trace context. Cloud Logging promotes recognized JSON fields into `LogEntry`
fields, so inspect `severity`, `trace`, and `spanId` at the entry level, not
only inside `jsonPayload`. See [structured logging][structured-logging].

When a result is missing, check the layer responsible:

| Symptom | Check |
| --- | --- |
| No application record | The selected logger's level/pipeline, output destination, and deployment collection |
| Missing request attributes | Whether the application selected the middleware-provided logger rather than only passing its context to the base logger |
| Missing trace correlation | The logging context, transport propagation, and resolved trace project ID |
| Trace field exists but no recorded trace is visible | Sampling, provider/exporter configuration, backend destination, and retention |
| Missing RPC completion logs | Whether upstream logging interceptors are installed. Native slogcpgrpc enrichment does not emit them |

For complete runnable applications, see the existing
[basic](../.examples/basic/main.go), [HTTP](../.examples/http-server/main.go),
[gRPC](../.examples/grpc/main.go), and [Pub/Sub](https://github.com/pjscruggs/slogcp-pubsub/blob/main/.examples/pubsub/main.go)
examples. Use the package guides for their detailed option and lifecycle
behavior.

[cloud-run-logging]:
  https://docs.cloud.google.com/run/docs/logging
[adapter-logger-selection]:
  https://github.com/pjscruggs/slogcp-grpc-adapter#selecting-a-request-scoped-logger
[otel-instrumentation]:
  https://opentelemetry.io/docs/languages/go/instrumentation/
[error-reporting-format]:
  https://docs.cloud.google.com/error-reporting/docs/formatting-error-messages
[structured-logging]:
  https://docs.cloud.google.com/logging/docs/structured-logging

[http-cloud-run-logging]:
  https://docs.cloud.google.com/run/docs/logging
  "Logging and viewing logs in Cloud Run  |  Google Cloud Documentation"
[http-log-entry]:
  https://docs.cloud.google.com/logging/docs/reference/v2/rest/v2/LogEntry
  "LogEntry  |  Cloud Logging  |  Google Cloud Documentation"

[pubsub-push]:
  https://docs.cloud.google.com/pubsub/docs/push
  "Push subscriptions  |  Pub/Sub  |  Google Cloud Documentation"
[pubsub-unwrapping]:
  https://docs.cloud.google.com/pubsub/docs/payload-unwrapping
  "Payload unwrapping for Pub/Sub push subscriptions  |  Google Cloud
  Documentation"
[go-client-observability]:
  https://docs.cloud.google.com/go/docs/observability
  "Enable telemetry signals in Go client libraries  |  Google Cloud
  Documentation"
