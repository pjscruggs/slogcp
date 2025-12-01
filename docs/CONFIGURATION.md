# slogcp Configuration

`slogcp` provides a Google Cloud friendly `slog.Handler` together with HTTP and gRPC integrations that derive request-scoped loggers, correlate records with Cloud Trace, and play well with OpenTelemetry instrumentation.

## Configuration Layers

`slogcp` resolves configuration in the following order:

1. **Defaults** - internal sensible defaults baked into each constructor.
2. **Environment variables** - evaluated when you call `slogcp.NewHandler`, `slogcphttp.Middleware`, or the `slogcpgrpc` helpers.
3. **Programmatic options** - `With...` overrides take precedence over everything else.

## Boolean ENV VARS

Boolean environment variables always accept any of `true`, `1`, `yes`, or `on` to enable and `false`, `0`, `no`, or `off` to disable (case-insensitive). Invalid values are ignored.

## Handler Setup

Construct the handler with `slogcp.NewHandler` and then wrap it with `slog.New`:

```go
handler, err := slogcp.NewHandler(os.Stdout,
	slogcp.WithLevel(slog.LevelInfo),
	slogcp.WithSourceLocationEnabled(true),
)
if err != nil {
	log.Fatalf("configure slogcp: %v", err)
}
defer handler.Close()

logger := slog.New(handler)
```

Key options:

| Option | Environment variable(s) | Default | Description |
| --- | --- | --- | --- |
| `WithLevel(slog.Level)` | `SLOGCP_LEVEL` | `info` | Minimum severity captured by the handler. |
| `WithLevelVar(*slog.LevelVar)` | (none) | current value of the supplied var | Shares a caller-managed `slog.LevelVar` so external config can adjust levels. |
| `WithSourceLocationEnabled(bool)` | `SLOGCP_SOURCE_LOCATION` | `false` | Populates `logging.googleapis.com/sourceLocation`. |
| `WithTime(bool)` | `SLOGCP_TIME` | Enabled outside Cloud Run, Cloud Run Jobs, Cloud Functions, and App Engine; disabled on those runtimes | Emits a top-level `time` field so logs carry RFC3339 timestamps. |
| `WithStackTraceEnabled(bool)` | `SLOGCP_STACK_TRACE_ENABLED` | `false` | Enables automatic stack capture at or above `StackTraceLevel`. |
| `WithStackTraceLevel(slog.Level)` | `SLOGCP_STACK_TRACE_LEVEL` | `error` | Threshold for automatic stacks. |
| `WithTraceProjectID(string)` | `SLOGCP_TRACE_PROJECT_ID`, `SLOGCP_PROJECT_ID`, `GOOGLE_CLOUD_PROJECT` | detected at runtime | Supplies the project ID used when formatting trace fields. |
| `WithTraceDiagnostics(slogcp.TraceDiagnosticsMode)` | `SLOGCP_TRACE_DIAGNOSTICS` | `warn` | Controls how slogcp surfaces trace correlation issues. Accepts `off`, `warn`/`warn_once`, or `strict` (which fails handler creation when no project can be detected). |
| `WithSeverityAliases(bool)` | `SLOGCP_SEVERITY_ALIASES` | `true` on Cloud Run (services), Cloud Run Jobs, Cloud Functions, and App Engine; otherwise `false` | Emits single-letter Cloud Logging severity aliases ("I", "E", etc.). Using these aliases saves about 1ns of JSON marshaling time per log entry, and has no effect the final Cloud Logging LogEntry. |
| `WithRedirectToStdout()` / `WithRedirectToStderr()` | `SLOGCP_TARGET` | `stdout` | Chooses the output destination. |
| `WithRedirectToFile(path)` | `SLOGCP_TARGET` (`file:<path>`) | (disabled) | Writes structured logs to a file (append mode); slogcp trims whitespace and uses the path verbatim. |
| `WithRedirectWriter(io.Writer)` | (none) | constructor writer | Uses any writer you supply without taking ownership; path parsing is left to the writer. |
| `WithReplaceAttr(func)` | (none) | (none) | Mutates or removes attributes before encoding. |
| `WithMiddleware(slogcp.Middleware)` | (none) | (none) | Wraps the handler with custom middleware. |
| `WithAttrs([]slog.Attr)` / `WithGroup(string)` | (none) | (none) | Adds fixed attributes or an initial group. |
| `WithInternalLogger(*slog.Logger)` | (none) | discarding text logger | Receives configuration warnings. |

Additional notes:
- File targets: use `SLOGCP_TARGET=file:<path>` (for example, `file:/var/log/app.json` on Linux/macOS or `file:C:\\logs\\app.json` on Windows). slogcp trims surrounding whitespace and passes the remaining path directly to `os.OpenFile` in append mode; it does not create parent directories or rewrite the string. Invalid values still trigger `ErrInvalidRedirectTarget` during handler construction so misconfigurations surface early.
- When you choose `WithRedirectWriter`, slogcp does not look at file paths at all; configure any file destination on the writer itself (for example, `*os.File` or a rotation helper like lumberjack).
- When logging to a file, `Handler.ReopenLogFile` rotates the owned descriptor after external tools move the file. Always call `Close` during shutdown to flush buffers and release writers.
- `Handler.LevelVar()` exposes the internal `slog.LevelVar`. You can adjust levels at runtime via `SetLevel` or share the var with other handlers.
- `WithSeverityAliases` controls whether JSON carries the terse severity names; Cloud Logging still renders the full names in the console. slogcp enables the aliases by default only on Cloud Run (services/jobs), Cloud Functions, and App Engine deployments.
- `WithTime` defaults mirror Cloud Logging expectations: timestamps are omitted on the same managed GCP runtimes (Cloud Run, Cloud Functions, App Engine) and included elsewhere. When slogcp emits a timestamp it preserves the nanosecond precision provided by `slog`.
- slogcp always validates trace correlation fields before emitting them. When no Cloud project ID can be resolved, the handler omits `logging.googleapis.com/trace` entirely (falling back to the `otel.*` keys) to avoid shipping malformed data. On managed runtimes where the Cloud Logging agent auto-prefixes trace IDs (Cloud Run services/jobs, Cloud Functions, App Engine) slogcp still emits the bare trace ID so existing deployments keep their correlation links. Use `WithTraceDiagnostics`/`SLOGCP_TRACE_DIAGNOSTICS` to upgrade these checks from "warn once" to `strict` or disable them with `off`.
- `slogcp.ContextWithLogger` and `slogcp.Logger` stash and recover request-scoped loggers. The HTTP and gRPC integrations call these helpers automatically.

## Async wrapper (`github.com/pjscruggs/slogcp/slogcpasync`)

`slogcp` stays synchronous by default. When you want to buffer or drop under load, wrap the handler with `slogcpasync`:

```go
handler, _ := slogcp.NewHandler(os.Stdout)
async := slogcpasync.Wrap(handler,
	slogcpasync.WithQueueSize(4096),
	slogcpasync.WithDropMode(slogcpasync.DropModeDropNewest),
)
logger := slog.New(async)
```

When you prefer environment-driven opt-in, combine `WithEnabled(false)` with `WithEnv()` (for example via `slogcp.WithMiddleware(slogcpasync.Middleware(...))`). Supported knobs:

| Option | Environment variable | Default | Description |
| --- | --- | --- | --- |
| `WithEnabled(bool)` | `SLOGCP_ASYNC_ENABLED` | `true` once the wrapper is added | Enables or disables the async wrapper entirely. |
| `WithQueueSize(int)` | `SLOGCP_ASYNC_QUEUE_SIZE` | `block: 2048`, `drop_newest: 512`, `drop_oldest: 1024` | Channel capacity for queued records. `0` uses an unbuffered channel. |
| `WithDropMode(slogcpasync.DropMode)` | `SLOGCP_ASYNC_DROP_MODE` | `block` | Overflow policy: `block`, `drop_newest`, or `drop_oldest`. |
| `WithWorkerCount(int)` | `SLOGCP_ASYNC_WORKERS` | `block: 1`, `drop_newest: 1`, `drop_oldest: 1` | Number of goroutines draining the queue. |
| `WithBatchSize(int)` | (none) | `block: 1`, `drop_newest: 1`, `drop_oldest: 1` | Records a worker drains per wake-up; values less than `1` clamp to the mode default. |
| `WithFlushTimeout(time.Duration)` | `SLOGCP_ASYNC_FLUSH_TIMEOUT` | (none) | Optional timeout for `Close`; returns `ErrFlushTimeout` if workers never finish. |
| `WithOnDrop(func)` | (none) | (none) | Callback invoked when a record is dropped (useful for metrics). |
| `slogcp.WithAsync(opts...)` | (none) | (disabled) | Convenience option that wraps handlers in slogcpasync using tuned per-mode defaults; supply slogcpasync options to override. |
| `slogcp.WithAsyncOnFileTargets(opts...)` | (none) | (disabled) | Like `WithAsync`, but only wraps handlers that write to files, leaving stdout/stderr synchronous. |
| `WithEnv()` | reads all of the above | (none) | Overlays any `SLOGCP_ASYNC_*` values onto the provided config. |

### Severity Levels

`slogcp` extends `log/slog` levels so they line up with [Google Cloud Logging's severities](https://docs.cloud.google.com/logging/docs/reference/v2/rest/v2/LogEntry#LogSeverity) (`DEBUG`, `INFO`, `NOTICE`, `WARNING`, `ERROR`, `CRITICAL`, `ALERT`, `EMERGENCY`, and `DEFAULT`). The exported constants in [`levels.go`](../levels.go) (for example `slogcp.LevelNotice`, `slogcp.LevelAlert`) and helper functions such as `slogcp.DefaultContext`, `slogcp.NoticeContext`, and `slogcp.AlertContext` make it easy to emit those severities directly.

`DEFAULT` is treated specially. Records written with `slogcp.LevelDefault` (including the `slogcp.Default`/`DefaultContext` helpers) will **ALWAYS** be logged. Even if you configure `WithLevel(slog.LevelWarn)` or set `SLOGCP_LEVEL=error`, default-severity records will still be delivered to Cloud Logging. This is done to respect GCP's intent that `DEFAULT` represents "no assigned severity level" and to provide a convenient way to debug issues relating to severity filtering.

All other severities retain their natural order relative to the standard slog levels: `DEBUG` < `INFO` < `NOTICE` < `WARNING` < `ERROR` < `CRITICAL` < `ALERT` < `EMERGENCY` < `DEFAULT`.


## HTTP Integration (`github.com/pjscruggs/slogcp/slogcphttp`)

Use `http.Middleware` to wrap servers and `http.Transport` to instrument clients:

```go
mw := slogcphttp.Middleware(
	slogcphttp.WithLogger(logger),
)

transport := slogcphttp.Transport(
	http.DefaultTransport,
	slogcphttp.WithLegacyXCloudInjection(true),
)
```

### Server Middleware

`Middleware` derives a logger per request, attaches it to the context (retrievable with `slogcp.Logger`), and records a `RequestScope` with method, route, latency, status, and peer metadata. When `WithOTel(true)` (the default) is in effect it composes `otelhttp.NewHandler` so OpenTelemetry spans are created automatically. No request logs are emitted; the middleware simply enriches application logs produced by your handlers.

Important options:

| Option | Description |
| --- | --- |
| `WithLogger(*slog.Logger)` | Sets the base logger used to derive request loggers. Defaults to `slog.Default()`. |
| `WithProjectID(string)` | Overrides the project ID used for Cloud Trace correlation. |
| `WithPropagators(propagation.TextMapPropagator)` | Customizes the propagator used to extract trace context when no span exists. |
| `WithTracePropagation(bool)` | Enables or disables extraction of incoming trace headers. Defaults to `true`. |
| `WithTracerProvider(trace.TracerProvider)` | Supplies the tracer provider passed to `otelhttp`. |
| `WithPublicEndpoint(bool)` | Forwards the public-endpoint hint to `otelhttp`. |
| `WithOTel(bool)` | Enables or disables wrapping with `otelhttp`. |
| `WithSpanNameFormatter(otelhttp.SpanNameFormatter)` / `WithFilter(otelhttp.Filter)` | Mirrors the underlying `otelhttp` hooks. |
| `WithAttrEnricher(func(*http.Request, *RequestScope) []slog.Attr)` | Adds custom attributes to the request logger. |
| `WithAttrTransformer(func([]slog.Attr, *http.Request, *RequestScope) []slog.Attr)` | Mutates attributes before they are applied. |
| `WithRouteGetter(func(*http.Request) string)` | Supplies a route template extractor (for example, from your mux). |
| `WithClientIP(bool)` | Toggles inclusion of `network.peer.ip` (enabled by default). |
| `WithIncludeQuery(bool)` | Opts into logging raw query strings (disabled by default). |
| `WithUserAgent(bool)` | Opts into logging the `User-Agent` string (disabled by default). |
| `WithHTTPRequestAttr(bool)` | Enables automatic addition of the Cloud Logging `httpRequest` payload to the derived logger. Disabled by default so applications must opt in. |

`RequestScope` captures derived metadata and can be retrieved with `slogcphttp.ScopeFromContext(ctx)`. The same helper works for outbound requests instrumented by `Transport`. To emit [the Cloud Logging `httpRequest` payload](https://docs.cloud.google.com/logging/docs/reference/v2/rest/v2/LogEntry#HttpRequest) you can either opt in globally via `slogcphttp.WithHTTPRequestAttr(true)` or attach the attribute ad hoc:

```go
logger := slogcp.Logger(ctx)
logger.InfoContext(ctx, "served",
	slogcphttp.HTTPRequestAttrFromContext(ctx, r),
)
```

If you prefer more control, `slogcphttp.HTTPRequestAttr` accepts the explicit `scope` value, and `slogcphttp.HTTPRequestEnricher` implements `AttrEnricher` for use with `WithAttrEnricher`. Outside of the middleware, `slogcp.HTTPRequestFromRequest(req)` creates a Cloud Logging payload from any standard library `*http.Request`.

### HTTP Client Transport

`Transport(base, opts...)` injects W3C trace headers (and, optionally, `X-Cloud-Trace-Context`) and derives a child logger for outbound requests. It reuses the same `Option` type as the middleware so you can share configuration slices across server and client instrumentation. Highlights include:

| Option | Description |
| --- | --- |
| `WithLogger(*slog.Logger)` | Sets the base logger for outbound requests. When omitted, the transport falls back to `slogcp.Logger(ctx)` or `slog.Default()`. |
| `WithProjectID(string)` | Controls the project ID used when computing Cloud Trace correlation fields. |
| `WithPropagators(propagation.TextMapPropagator)` | Overrides the propagator used for trace header injection. |
| `WithTracePropagation(bool)` | Enables or disables outbound trace header injection. Defaults to `true`. |
| `WithAttrEnricher` / `WithAttrTransformer` | Allow custom attribute enrichment or redaction for outbound requests. |
| `WithClientIP(bool)` | Toggles inclusion of the resolved host address as `network.peer.ip`. Enabled by default. |
| `WithIncludeQuery(bool)` / `WithUserAgent(bool)` | Control whether the query string and user agent are recorded on derived loggers. |
| `WithLegacyXCloudInjection(bool)` | Synthesizes the legacy `X-Cloud-Trace-Context` header in addition to W3C trace headers. |

Client requests also populate a `RequestScope`, making latency, status, and payload sizes available via `ScopeFromContext`. `InjectTraceContextMiddleware` remains available for the rare case where you disable `otelhttp` and still need to recognize `X-Cloud-Trace-Context` manually.

## gRPC Integration (`github.com/pjscruggs/slogcp/slogcpgrpc`)

`grpc.UnaryServerInterceptor`, `grpc.StreamServerInterceptor`, `grpc.UnaryClientInterceptor`, and `grpc.StreamClientInterceptor` derive per-RPC loggers, propagate trace context, and capture method/service/latency/status metadata. Each interceptor stores the logger in the context (so `slogcp.Logger(ctx)` works inside handlers) and records a `RequestInfo` structure that you can retrieve later with `slogcpgrpc.InfoFromContext`.

Important options:

| Option | Description |
| --- | --- |
| `WithLogger(*slog.Logger)` | Sets the base logger for derived RPC loggers. Defaults to `slog.Default()`. |
| `WithProjectID(string)` | Overrides the project used for trace correlation. |
| `WithPropagators(propagation.TextMapPropagator)` | Custom propagator for metadata extraction (server) or injection (client). |
| `WithTracePropagation(bool)` | Enables or disables trace extraction/injection on servers and clients. Defaults to `true`. |
| `WithTracerProvider(trace.TracerProvider)` | Passed to the otelgrpc StatsHandler when OpenTelemetry instrumentation is enabled. |
| `WithPublicEndpoint(bool)` | Marks the service as public for telemetry. |
| `WithOTel(bool)` | Enables or disables otelgrpc StatsHandlers (enabled by default). |
| `WithSpanAttributes(attribute.KeyValue...)` / `WithFilter(otelgrpc.Filter)` | Mirrors otelgrpc configuration knobs. |
| `WithAttrEnricher(func(context.Context, *RequestInfo) []slog.Attr)` / `WithAttrTransformer(func(context.Context, []slog.Attr, *RequestInfo) []slog.Attr)` | Customizes logger attributes before they are applied. |
| `WithPeerInfo(bool)` | Toggles `net.peer.ip` enrichment for inbound RPCs (enabled by default). |
| `WithPayloadSizes(bool)` | Controls request/response byte counting (enabled by default). |
| `WithLegacyXCloudInjection(bool)` | Synthesizes `x-cloud-trace-context` on outgoing RPCs in addition to standard OpenTelemetry headers. |

`RequestInfo` tracks service/method names, stream kinds, latencies, status codes, peer addresses, and (when enabled) message sizes. Use it to enrich application logs or emit custom metrics without recomputing the values.

Helpers:

- `ServerOptions(opts ...Option)` returns a `[]grpc.ServerOption` containing an otelgrpc StatsHandler (when `WithOTel(true)`) plus both server interceptors.
- `DialOptions(opts ...Option)` returns matching client interceptors and StatsHandler.
- `InfoFromContext(ctx)` retrieves the `RequestInfo` captured by the interceptors so handlers can inspect RPC metadata at any point.

## Trace Propagation Defaults

Importing `slogcp` installs a composite OpenTelemetry propagator that understands both W3C Trace Context and Google's legacy `X-Cloud-Trace-Context` header (read-only). Set `SLOGCP_DISABLE_PROPAGATOR_AUTOSET=true` before importing the package if you prefer to manage `otel.SetTextMapPropagator` yourself; call `slogcp.EnsurePropagation()` if you need to reapply the library's defaults later in process startup.
