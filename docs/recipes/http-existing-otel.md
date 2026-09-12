# Add slogcp to an HTTP application with OpenTelemetry

## Start with the application's existing tracing setup

Use this recipe when an HTTP application already has an OpenTelemetry tracer
provider, propagators, and an export pipeline. The intended result is structured
application logs correlated to the spans that setup creates. Request-scoped HTTP
fields are an optional addition.

Keep the existing instrumentation's sampling, filters, public-endpoint policy,
operation names, and metrics configuration. Changing the logging handler does
not require replacing any of them. This recipe assumes an outer `otelhttp`
wrapper creates the request span before calling application handlers.

From the consuming application's Go module, add the core library.

```sh
go get github.com/pjscruggs/slogcp/v2
```

Check the minimum Go requirement in [`go.mod`](../../go.mod). The application's
existing [otelhttp configuration][otelhttp] remains its source of tracing
policy.

## Change the logging output first

This application helper includes its required imports. It is a source-file
excerpt, not a complete server. Call it once at startup with your writer and the
ID of the project that owns your traces. For example, a deployment can pass
`os.Stdout` as `out` after importing `os` in its startup code.

```go
package app

import (
	"io"
	"log/slog"

	"github.com/pjscruggs/slogcp/v2"
)

func newAppLogger(
	out io.Writer, projectID string,
) (*slog.Logger, *slogcp.Handler, error) {
	handler, err := slogcp.NewHandler(out,
		slogcp.WithRedirectWriter(out),
		slogcp.WithTraceProjectID(projectID),
	)
	if err != nil {
		return nil, nil, err
	}
	logger := slog.New(handler).With("component", "orders")
	return logger, handler, nil
}
```

Handle the returned error and retain the handler until shutdown. Stop logging
producers before calling its `Close` method. `WithRedirectWriter` keeps
ownership of the supplied writer with the application. See the [shutdown
guidance](../USAGE.md#manage-configuration-and-shutdown).

At the `slog.New` call, preserve any existing handler wrappers for filtering,
redaction, or fan-out, and reapply the application's shared attributes. A newly
constructed logger cannot recover attributes or wrappers from an old logger. The
example binds `component` to the common base so derived loggers inherit it.

The core handler is sufficient if the application's routes already log through
that base with `logger.InfoContext(r.Context(), ...)` and need no additional
HTTP fields. Keep the existing HTTP wrapper and provider/export setup. Passing
the current context lets slogcp read the active span without creating one.

## Add request fields inside the existing HTTP wrapper

The next source-file excerpt adds request enrichment. `base` is the configured,
ungrouped logger from startup. `tracer` comes from the application's existing
provider. `instrument` is its existing `func(http.Handler) http.Handler`
wrapper, such as the function returned by `otelhttp.NewMiddleware` with the
application's current options. Apply that wrapper exactly once.

```go
package app

import (
	"log/slog"
	"net/http"

	"github.com/pjscruggs/slogcp/v2"
	"github.com/pjscruggs/slogcp/v2/slogcphttp"
	"go.opentelemetry.io/otel/trace"
)

func newHTTPHandler(
	base *slog.Logger,
	tracer trace.Tracer,
	instrument func(http.Handler) http.Handler,
) http.Handler {
	mux := http.NewServeMux()
	serveOrders := func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		base.InfoContext(ctx, "base log")

		requestLogger := slogcp.Logger(ctx).With("order_id", "order-42")
		requestLogger.InfoContext(ctx, "request log")

		childCtx, span := tracer.Start(ctx, "load order")
		defer span.End()
		requestLogger.InfoContext(childCtx, "child log")
		w.WriteHeader(http.StatusNoContent)
	}
	mux.HandleFunc("GET /orders", serveOrders)

	enriched := slogcphttp.Middleware(
		slogcphttp.WithLogger(base),
		slogcphttp.WithOTel(false),
		slogcphttp.WithTracePropagation(false),
	)(mux)
	return instrument(enriched)
}
```

Use the returned handler as your existing server's `Handler`. If your current
code calls `otelhttp.NewHandler` directly, put `enriched` inside that existing
call instead. Retain its operation name and options.

`WithOTel(false)` disables slogcphttp's own instrumentation wrapper.
`WithTracePropagation(false)` leaves header extraction to the outer wrapper,
including its filtering and trust decisions. The handler can still correlate
logs with a span already in the context. No global propagator is changed, and
this recipe does not call `EnsurePropagation`.

The three log calls demonstrate different choices.

- `base log` retains `component` and receives the request span context. It does
  not acquire the request logger's HTTP fields just by receiving that context.
- `request log` uses the logger selected from the request context. It retains
  `component` and adds `http.method`, `http.target`, and `order_id`.
- `child log` retains those fields but uses the child span passed at the call.
  Its trace stays the same and its span ID changes.

An injected worker can select `slogcp.Logger(ctx)` when it wants the contextual
logger, or use [an explicit fallback][context-fallback] when no contextual
logger exists. Derive from the common base to retain shared attributes and
policies. Keep it ungrouped if HTTP fields should be top-level. The [HTTP
package guide](../../slogcphttp/README.md) covers optional fields and route
extraction.

## Verify attributes and span ownership

Run `go mod tidy` and `go test ./...` in your application module. In a local
test, use a synchronized in-memory log writer and an OTel in-memory span
exporter. Send `GET /orders` through the real composed handler with a valid
`traceparent` header accepted by your existing propagator.

Check all three log entries for `component`. Check the two contextual entries
for HTTP fields and `order_id`, and check that the base entry lacks those
fields. With a trace project configured, compare `logging.googleapis.com/trace`
and `logging.googleapis.com/spanId` against the exported request and child
spans. Expect one server span and one child span for this route, with the child
parented to the server span. Compare against the application's
instrumentation-only baseline to detect a duplicate wrapper. Also exercise any
existing OTel filter or public-endpoint policy so enrichment cannot silently
bypass it.

The middleware adds no access record. In-flight application logs are not
response completion logs. Cloud Run supplies platform request logs and collects
container stdout/stderr, as described in [Cloud Run logging][cloud-run]. Other
deployments need their own collection path.

Local checks verify JSON, context selection, and span relationships. After
deployment, separately verify collection and trace export. Cloud Logging
[promotes recognized JSON fields][structured] into entry fields such as `trace`
and `spanId`. A correlation field alone does not establish that the trace was
exported or retained.

[otelhttp]:
  https://pkg.go.dev/go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp
[cloud-run]: https://docs.cloud.google.com/run/docs/logging
[structured]: https://docs.cloud.google.com/logging/docs/structured-logging
[context-fallback]:
  ../USAGE.md#use-a-request-scoped-logger-with-an-explicit-fallback-when-needed
