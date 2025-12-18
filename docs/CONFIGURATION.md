# slogcp Configuration

`slogcp` provides a Google Cloud friendly `slog.Handler` together with HTTP and gRPC integrations that derive request-scoped loggers, correlate records with Cloud Trace, and play well with OpenTelemetry instrumentation.

## Configuration Layers

`slogcp` resolves configuration in the following order:

1. **Defaults** - internal sensible defaults baked into each constructor.
2. **Environment variables** - evaluated when you call `slogcp.NewHandler`, `slogcphttp.Middleware`, or the `slogcpgrpc` helpers.
3. **Programmatic options** - `With...` overrides take precedence over everything else.

## Boolean ENV VARS

Boolean environment variables are always parsed with [strconv.ParseBool](https://pkg.go.dev/strconv#ParseBool) which, "accepts `1`, `t`, `T`, `TRUE`, `true`, `True`, `0`, `f`, `F`, `FALSE`, `false`, `False`." Invalid values are ignored.

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
| `WithLevel(slog.Level)` | `SLOGCP_LEVEL` (fallback: `LOG_LEVEL`) | `info` | Minimum severity captured by the handler. |
| `WithLevelVar(*slog.LevelVar)` | `SLOGCP_LEVEL` (fallback: `LOG_LEVEL`) | `info` | Shares a caller-managed `slog.LevelVar` that slogcp initializes during handler construction so external config can adjust levels. |
| `WithSourceLocationEnabled(bool)` | `SLOGCP_SOURCE_LOCATION` | `false` | Populates `logging.googleapis.com/sourceLocation`. |
| `WithTime(bool)` | `SLOGCP_TIME` | Enabled outside Cloud Run, Cloud Run Jobs, Cloud Functions, and App Engine; also enabled for file targets on those runtimes; disabled on those runtimes when writing to stdout/stderr | Emits a top-level `time` field so logs carry RFC3339 timestamps. |
| `WithStackTraceEnabled(bool)` | `SLOGCP_STACK_TRACES` | `false` | Enables automatic stack capture at or above `StackTraceLevel`. |
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
- When you choose `WithRedirectWriter`, slogcp does not look at file paths at all; configure any file destination on the writer itself (for example, `*os.File` or a rotation helper like timberjack).
- When logging to a file, `Handler.ReopenLogFile` rotates the owned descriptor after external tools move the file. Always call `Close` during shutdown to flush buffers and release writers.
- `SLOGCP_LEVEL` is the preferred knob for minimum severity. When it is empty, slogcp also honours `LOG_LEVEL` so shared conventions still work. When you supply `WithLevelVar`, slogcp seeds the shared var using the same resolution rules.
- `Handler.LevelVar()` exposes the internal `slog.LevelVar`. You can adjust levels at runtime via `SetLevel` or share the var with other handlers.
- `WithSeverityAliases` controls whether JSON carries the terse severity names; Cloud Logging still renders the full names in the console. slogcp enables the aliases by default only on Cloud Run (services/jobs), Cloud Functions, and App Engine deployments.
- `WithTime` defaults mirror Cloud Logging expectations: timestamps are omitted on the same managed GCP runtimes (Cloud Run, Cloud Functions, App Engine) when writing to stdout/stderr, but file targets keep timestamps even there so rotated/shipped logs stay annotated. When slogcp emits a timestamp it preserves the nanosecond precision provided by `slog`.
- slogcp always validates trace correlation fields before emitting them. When no Cloud project ID can be resolved, the handler omits `logging.googleapis.com/trace` entirely (falling back to the `otel.*` keys) to avoid shipping malformed data. On managed runtimes where the Cloud Logging agent auto-prefixes trace IDs (Cloud Run services/jobs, Cloud Functions, App Engine) slogcp still emits the bare trace ID so existing deployments keep their correlation links. Use `WithTraceDiagnostics`/`SLOGCP_TRACE_DIAGNOSTICS` to upgrade these checks from "warn once" to `strict` or disable them with `off`.
- `slogcp.ContextWithLogger` and `slogcp.Logger` stash and recover request-scoped loggers. The HTTP and gRPC integrations call these helpers automatically.

## Async logging (`slogcpasync`)

`slogcp` writes synchronously to `stdout`/`stderr` by default.

### Defaults

When slogcp writes to a file target (`SLOGCP_TARGET=file:...` or `slogcp.WithRedirectToFile`), it buffers writes with `slogcpasync` automatically so disk I/O doesn't sit on hot paths. Whenever the async wrapper is in play (file targets by default, or `WithAsync`/`Wrap`), call `Close()` on shutdown so queued records flush.

### Tuning (or disabling) file buffering

Use `slogcp.WithAsyncOnFileTargets(...)` to change the wrapper options applied to file targets, including disabling the wrapper entirely:

```go
handler, _ := slogcp.NewHandler(nil,
	slogcp.WithRedirectToFile("app.json"),
	slogcp.WithAsyncOnFileTargets(
		slogcpasync.WithEnabled(false),
	),
)
defer handler.Close()
```

### Enabling async for non-file targets

To buffer `stdout`/`stderr` (or any other non-file target), you can have slogcp wrap the handler with `slogcp.WithAsync(...)`:

```go
handler, _ := slogcp.NewHandler(os.Stdout,
	slogcp.WithAsync(
		slogcpasync.WithQueueSize(4096),
		slogcpasync.WithDropMode(slogcpasync.DropModeDropNewest),
	),
)
defer handler.Close()
logger := slog.New(handler)
```

Or wrap any `slog.Handler` directly with `slogcpasync.Wrap(...)`:

```go
base := slog.NewJSONHandler(os.Stdout, nil)
async := slogcpasync.Wrap(base, slogcpasync.WithQueueSize(4096))
if closer, ok := async.(interface{ Close() error }); ok {
	defer closer.Close()
}
logger := slog.New(async)
```

### Options and environment variables

When you prefer environment-driven opt-in, combine `WithEnabled(false)` with `WithEnv()` (for example via `slogcp.WithMiddleware(slogcpasync.Middleware(...))`).

`slogcp.WithAsync(...)` and `slogcp.WithAsyncOnFileTargets(...)` already integrate the wrapper; avoid also adding `slogcpasync.Middleware` unless you deliberately want multiple queues.

| Option | Environment variable | Default | Description |
| --- | --- | --- | --- |
| `WithEnabled(bool)` | `SLOGCP_ASYNC` | `true` once the wrapper is added | Enables or disables the async wrapper entirely. |
| `WithQueueSize(int)` | `SLOGCP_ASYNC_QUEUE_SIZE` | `block: 2048`, `drop_newest: 512`, `drop_oldest: 1024` | Channel capacity for queued records. `0` uses an unbuffered channel. |
| `WithDropMode(slogcpasync.DropMode)` | `SLOGCP_ASYNC_DROP_MODE` | `block` | Overflow policy: `block`, `drop_newest`, or `drop_oldest`. |
| `WithWorkerCount(int)` | `SLOGCP_ASYNC_WORKERS` | `block: 1`, `drop_newest: 1`, `drop_oldest: 1` | Number of goroutines draining the queue. |
| `WithBatchSize(int)` | (none) | `block: 1`, `drop_newest: 1`, `drop_oldest: 1` | Records a worker drains per wake-up; values less than `1` clamp to the mode default. |
| `WithFlushTimeout(time.Duration)` | `SLOGCP_ASYNC_FLUSH_TIMEOUT` | (none) | Optional timeout for `Close`; returns `ErrFlushTimeout` if workers never finish. |
| `WithOnDrop(func)` | (none) | (none) | Callback invoked when a record is dropped (useful for metrics). |
| `slogcp.WithAsync(opts...)` | (none) | (disabled) | Convenience option that wraps handlers in slogcpasync using tuned per-mode defaults; supply slogcpasync options to override. |
| `slogcp.WithAsyncOnFileTargets(opts...)` | (none) | (enabled for file targets) | Tunes (or disables) slogcpasync settings for file targets while leaving stdout/stderr synchronous. |
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
| `WithPublicEndpointCorrelateLogsToRemote(bool)` | When `WithPublicEndpoint(true)` and `WithOTel(false)`, controls whether logs correlate to inbound trace headers (disabled by default). |
| `WithOTel(bool)` | Enables or disables wrapping with `otelhttp`. |
| `WithSpanNameFormatter(otelhttp.SpanNameFormatter)` / `WithFilter(otelhttp.Filter)` | Mirrors the underlying `otelhttp` hooks. |
| `WithAttrEnricher(func(*http.Request, *RequestScope) []slog.Attr)` | Adds custom attributes to the request logger. |
| `WithAttrTransformer(func([]slog.Attr, *http.Request, *RequestScope) []slog.Attr)` | Mutates attributes before they are applied. |
| `WithRouteGetter(func(*http.Request) string)` | Supplies a route template extractor (for example, from your mux). |
| `WithClientIP(bool)` | Toggles inclusion of `network.peer.ip` (enabled by default). In `ProxyModeGCLB`, the value is derived from `X-Forwarded-For`. |
| `WithTrustXForwardedProto(bool)` | Controls whether `http.scheme` is derived from `X-Forwarded-Proto` for logging. Defaults to enabled on Cloud Run services, Cloud Functions, and App Engine; otherwise disabled. |
| `WithProxyMode(ProxyMode)` | Enables proxy-aware parsing of `X-Forwarded-For` (and always trusts `X-Forwarded-Proto` for scheme). Disabled by default because forwarded headers are easy to spoof if requests can bypass your proxy. |
| `WithXForwardedForClientIPFromRight(int)` | In proxy-aware modes, selects the client IP position in `X-Forwarded-For` counting from the right (default: 2). |
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

#### Do you really need to log httpRequest?

- Managed runtimes already emit an automatic request log (with final status/size/latency) and also stamp `trace`/`spanId` onto every app log. The normal way to correlate is to pivot on `trace` so the app logs and the automatic request log show up together in the Cloud Logging UI/Trace viewer. Attaching `httpRequest` to application logs is therefore an opt-in, niche move for self-contained error/alert payloads or for services that *lack* an automatic request log.
- Opt-in middleware attachment uses a **lazy LogValuer**: the `httpRequest` is built at log time from the live `RequestScope`. While the request is in flight we intentionally suppress `httpRequest.status`, `httpRequest.responseSize`, and `httpRequest.latency` (Cloud Logging will still promote the `httpRequest`).
- For one-shot “access log” style entries, call `slogcphttp.HTTPRequestFromScope(scope)` to snapshot the current state into a Cloud Logging payload. Outside of the middleware, `slogcp.HTTPRequestFromRequest(req)` creates a Cloud Logging payload from any standard library `*http.Request`.

### HTTP Client Transport

`Transport(base, opts...)` injects W3C trace headers (and, optionally, `X-Cloud-Trace-Context`) and derives a child logger for outbound requests. It reuses the same `Option` type as the middleware so you can share configuration slices across server and client instrumentation. Highlights include:

| Option | Description |
| --- | --- |
| `WithLogger(*slog.Logger)` | Sets the base logger for outbound requests. When omitted, the transport falls back to `slogcp.Logger(ctx)` or `slog.Default()`. |
| `WithProjectID(string)` | Controls the project ID used when computing Cloud Trace correlation fields. |
| `WithPropagators(propagation.TextMapPropagator)` | Overrides the propagator used for trace header injection. |
| `WithTracePropagation(bool)` | Enables or disables outbound trace header injection. Defaults to `true`. |
| `WithAttrEnricher` / `WithAttrTransformer` | Allow custom attribute enrichment or redaction for outbound requests. |
| `WithClientIP(bool)` | Toggles inclusion of the resolved host address as `server.address` (and `server.port` when available). Enabled by default. |
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

## Pub/Sub Integration (`github.com/pjscruggs/slogcp/slogcppubsub`)

`slogcppubsub` provides helpers for carrying trace context across Pub/Sub boundaries via `pubsub.Message.Attributes`, and for deriving message-scoped loggers in pull subscribers (so `slogcp.Logger(ctx)` works inside receive handlers). Use `Inject` before publishing and `WrapReceiveHandler` when receiving; `Extract` / `ExtractAttributes` and `InfoFromContext` are available when you want manual control or need to inspect message metadata.

| Option | Default | Description |
| --- | --- | --- |
| `WithLogger(*slog.Logger)` | `slog.Default()` | Base logger used to derive per-message loggers (used by `WrapReceiveHandler`). |
| `WithProjectID(string)` | detected at runtime | Project used when formatting Cloud Logging trace correlation fields and `gcp.project_id` span attributes. |
| `WithSubscription(*pubsub.Subscriber)` | (unset) | Captures a trimmed subscription ID from `sub.ID()` for span/logger enrichment. |
| `WithSubscriptionID(string)` | (unset) | Sets the trimmed subscription ID used for span/logger enrichment. |
| `WithTopic(*pubsub.Publisher)` | (unset) | Captures a trimmed topic ID from `topic.ID()` for span/logger enrichment. |
| `WithTopicID(string)` | (unset) | Sets the trimmed topic ID used for span/logger enrichment. |
| `WithOTel(bool)` | `true` | Enables/disables creation of an application-level consumer span around message processing. |
| `WithSpanStrategy(slogcppubsub.SpanStrategy)` | `SpanStrategyAlways` | Controls when consumer spans are started (`SpanStrategyAuto` skips when a local span is already active; `SpanStrategyAlways` always starts). |
| `WithTracerProvider(trace.TracerProvider)` | global tracer provider | Tracer provider used when consumer spans are created. |
| `WithSpanName(string)` | `pubsub.process` | Span name used when creating consumer spans. |
| `WithSpanAttributes(attribute.KeyValue...)` | (none) | Additional OpenTelemetry attributes appended to consumer spans. |
| `WithPublicEndpoint(bool)` | `false` | Treats producers as untrusted: starts a new root trace and links extracted remote context instead of parenting. |
| `WithPublicEndpointCorrelateLogsToRemote(bool)` | `false` | When `WithPublicEndpoint(true)` and no local span is created (for example, `WithOTel(false)`), controls whether logs correlate to extracted remote trace context. |
| `WithPropagators(propagation.TextMapPropagator)` | `otel.GetTextMapPropagator()` | Propagator used for attribute injection/extraction; passing `nil` disables propagator-based extraction/injection. |
| `WithTracePropagation(bool)` | `true` | Enables/disables trace context extraction and injection via message attributes. |
| `WithBaggagePropagation(bool)` | `false` | Enables/disables baggage injection/extraction via message attributes when the propagator supports it. |
| `WithCaseInsensitiveExtraction(bool)` | `false` | Enables case-insensitive lookup for propagation keys during extraction. |
| `WithInjectOnlyIfSpanPresent(bool)` | `false` | Only injects when a valid span context is present on `ctx`. |
| `WithLogMessageID(bool)` | `false` | Adds `messaging.message.id` to derived loggers (high-cardinality). |
| `WithLogOrderingKey(bool)` | `false` | Adds `messaging.gcp_pubsub.message.ordering_key` to derived loggers (can be high-cardinality). |
| `WithLogDeliveryAttempt(bool)` | `true` | Adds `messaging.gcp_pubsub.message.delivery_attempt` to derived loggers when present. |
| `WithLogPublishTime(bool)` | `false` | Adds `pubsub.message.publish_time` to derived loggers. |
| `WithAttrEnricher(func(context.Context, *pubsub.Message, *MessageInfo) []slog.Attr)` | (none) | Appends additional attributes to the derived message logger. |
| `WithAttrTransformer(func(context.Context, []slog.Attr, *pubsub.Message, *MessageInfo) []slog.Attr)` | (none) | Mutates/redacts the derived message logger attribute slice before it is applied. |
| `WithGoogClientCompat(bool)` | `false` | Enables both extraction fallback and injection compatibility via `googclient_`-prefixed keys. |
| `WithGoogClientExtraction(bool)` | `false` | Enables extraction from `googclient_`-prefixed keys when standard keys are absent. |
| `WithGoogClientInjection(bool)` | `false` | Enables injection of `googclient_`-prefixed keys in addition to standard keys. |

## Trace Propagation Defaults

Importing `slogcp` installs a composite OpenTelemetry propagator that understands both W3C Trace Context and Google's legacy `X-Cloud-Trace-Context` header (read-only). Set `SLOGCP_PROPAGATOR_AUTOSET=false` before importing the package if you prefer to manage `otel.SetTextMapPropagator` yourself; call `slogcp.EnsurePropagation()` if you need to reapply the library's defaults later in process startup.
