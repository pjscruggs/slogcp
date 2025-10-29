# slogcp Configuration

This guide explains how to configure `github.com/pjscruggs/slogcp` and its HTTP and gRPC integrations. The handler emits Cloud Logging-compatible JSON to an `io.Writer`; Google Cloud's collectors (Cloud Logging Agent, Cloud Run, GKE, etc.) can ingest this output from stdout/stderr or any other transport you configure.

## Layered Configuration Model

`slogcp` applies settings in three layers:

1. **Defaults** – internal sensible defaults.
2. **Environment variables** – read during `slogcp.NewHandler(defaultWriter)` and the middleware/interceptor constructors.
3. **Programmatic options** – `With…` functions override both defaults and environment variables.

> **Boolean environment values** – each boolean flag accepts `true`, `1`, `yes`, or `on` to enable and `false`, `0`, `no`, or `off` to disable (case-insensitive). Invalid values are ignored.

## Handler Setup (`slogcp.NewHandler`)

Create a handler with your preferred writer and options, then pass it to `slog.New`:

```go
handler, err := slogcp.NewHandler(os.Stdout,
	slogcp.WithLevel(slog.LevelInfo),
	slogcp.WithSourceLocationEnabled(true),
)
if err != nil {
	log.Fatalf("set up slogcp handler: %v", err)
}
defer handler.Close()

logger := slog.New(handler)
```

### Core Options and Environment Variables

| Option | Environment variable(s) | Default | Description |
| --- | --- | --- | --- |
| `WithLevel(slog.Level)` | `LOG_LEVEL` | `info` | Minimum severity captured by the handler. Accepts textual (`debug`, `notice`, etc.) or integer `slog.Level` values. |
| `WithLevelVar(*slog.LevelVar)` | (none) | `nil` | Shares an existing `slog.LevelVar` with the handler so multiple loggers can adjust level in lockstep. The handler still applies `WithLevel`/`LOG_LEVEL` to seed the shared variable. |
| `WithSourceLocationEnabled(bool)` | `LOG_SOURCE_LOCATION` | `false` | When enabled, populates `logging.googleapis.com/sourceLocation`. |
| `WithTime(bool)` | `LOG_TIME` | `false` | Emits the top-level RFC3339Nano `time` field instead of relying on Cloud Logging to supply a timestamp. |
| `WithStackTraceEnabled(bool)` | `LOG_STACK_TRACE_ENABLED` | `false` | Turns on automatic stack capture at or above `StackTraceLevel`. |
| `WithStackTraceLevel(slog.Level)` | `LOG_STACK_TRACE_LEVEL` | `error` | Severity threshold for automatic stack traces. |
| `WithTraceProjectID(string)` | `SLOGCP_TRACE_PROJECT_ID` → `SLOGCP_PROJECT_ID` → `GOOGLE_CLOUD_PROJECT` | detected via runtime info | Chooses the project used when formatting `logging.googleapis.com/trace`. Fallback resolution matches the listed order. |
| `WithRedirectToStdout()` / `WithRedirectToStderr()` | `SLOGCP_REDIRECT_AS_JSON_TARGET=stdout|stderr`<br>`SLOGCP_LOG_TARGET=stdout|stderr` | `stdout` | Redirects JSON output to the desired stream. |
| `WithRedirectToFile(path)` | `SLOGCP_REDIRECT_AS_JSON_TARGET=file:/path/to/log.json`<br>`SLOGCP_LOG_TARGET=file` | none | Opens (append mode) and writes to the specified path. `SLOGCP_LOG_TARGET=file` requires the `file:` env to supply the path. |
| `WithRedirectWriter(io.Writer)` | (none) | inherited from constructor | Uses any writer you supply (for example lumberjack, sockets, or buffers). |
| `WithReplaceAttr(func)` | (none) | `nil` | Mutates or removes attributes just before encoding. Returning the zero `slog.Attr{}` drops the attribute. |
| `WithMiddleware(slogcp.Middleware)` | (none) | none | Wraps the core handler with additional `slog.Handler` middleware. Added middleware runs outside-in (last option wraps first). |
| `WithAttrs([]slog.Attr)` / `WithGroup(string)` | (none) | none | Adds initial attributes or a starting group to every record. |
| `WithInternalLogger(*slog.Logger)` | (none) | silent text logger | Receives configuration warnings (for example invalid env values, file reopen errors). Useful for surfacing misconfiguration. |

`SLOGCP_LOG_TARGET=gcp` returns an error because the handler does not use the Cloud Logging API client. Pick a JSON destination instead (stdout/stderr/file/custom writer).

If neither an option nor an environment variable supplies a writer, the handler uses the `defaultWriter` argument from `NewHandler`; if that is `nil`, it falls back to `os.Stdout`.

### File Destinations and Rotation

`WithRedirectToFile` and `SLOGCP_REDIRECT_AS_JSON_TARGET=file:…` open the path in append mode (`0644`). When the handler owns the file, `handler.ReopenLogFile()` can be called (for example during a SIGHUP handler) to cooperate with external rotation tools. Any writer that implements `io.Closer` is closed when you call `handler.Close()`.

### Runtime Metadata and Labels

`slogcp.DetectRuntimeInfo()` runs during handler construction and adds environment-derived metadata automatically:

- `logging.googleapis.com/labels` receives platform-specific identifiers (Cloud Run revisions, Cloud Functions names, GCE zones, Kubernetes metadata, etc.).
- `serviceContext` is populated when appropriate (service/version pairs for Cloud Run, Cloud Functions, App Engine, and others).
- If no explicit trace project is configured, the detected project populates `TraceProjectID` so trace correlation still works out of the box.

You can supplement or override labels by logging into the reserved group explicitly:

```go
logger.WithGroup(slogcp.LabelsGroup).Info("ready", slog.String("team", "observability"))
```

### Handler Lifecycle

- `handler.Close()` flushes buffered output, closes owned files, and closes custom writers implementing `io.Closer`. It is idempotent.
- `handler.ReopenLogFile()` reopens the managed file writer after log rotation.
- All closing errors are emitted through the internal diagnostic logger when provided.
- `handler.SetLevel(slog.Level)` and `handler.LevelVar()` expose runtime level control without reinstalling the handler. Use these hooks to adjust verbosity in response to configuration changes or admin endpoints.

### Trace Helpers and Context Integration

- `slogcp.TraceAttributes(ctx, projectID)` returns trace-aware attributes (`logging.googleapis.com/trace`, `logging.googleapis.com/spanId`, etc.) for composing request loggers.
- `slogcp.ContextWithLogger(ctx, logger)` embeds a request-scoped logger in the context; `slogcp.Logger(ctx)` retrieves it (falling back to `slog.Default()`). Both HTTP and gRPC integrations use these helpers when `AttachLogger` is enabled.

### Stack Traces and Error Fields

When an attribute value implements `error`, the handler emits the error message, captures the Go type in `error_type`, and, if stack tracing is enabled for the level, adds the stack to `stack_trace`. Stack traces gathered from wrapped errors (via `ExtractStack`) are preferred; otherwise the handler captures a best-effort runtime stack.

## JSON Payload Layout

Every record is encoded as a single JSON object. Key fields include:

- `severity` – Google Cloud severity code using single-letter aliases (`D`, `I`, `N`, `W`, `E`, `C`, `A`), the abbreviated `EMERG` for emergency, and `DEFAULT` for unspecified logs.
- `message` – record message (augmented with the error message when `error_type` is present).
- `time` – RFC3339Nano timestamp in UTC when `WithTime`/`LOG_TIME` is true.
- `logging.googleapis.com/sourceLocation` – present when source logging is enabled.
- `logging.googleapis.com/trace`, `logging.googleapis.com/spanId`, `logging.googleapis.com/trace_sampled` – written when the context carries a valid OpenTelemetry span and a project can be determined.
- `otel.trace_id`, `otel.span_id`, `otel.trace_sampled` – fallback keys when no trace project is known.
- `logging.googleapis.com/labels` – merged runtime labels plus any emitted under that group.
- `serviceContext` – auto-populated service metadata when detected or explicitly provided.
- `httpRequest` – populated when an attached attribute is a `*slogcp.HTTPRequest` (HTTP middleware does this automatically).
- `error_type` / `stack_trace` – present when errors or stack tracing are enabled.

Extended severity support is exposed through `slogcp.Level` constants. They map cleanly onto Google Cloud severities while remaining `slog.Level` compatible:

| Constant | Numeric value | Cloud Logging severity |
| --- | --- | --- |
| `slogcp.LevelDefault` | 30 | `DEFAULT` |
| `slogcp.LevelDebug` | -4 | `DEBUG` |
| `slogcp.LevelInfo` | 0 | `INFO` |
| `slogcp.LevelNotice` | 2 | `NOTICE` |
| `slogcp.LevelWarn` | 4 | `WARNING` |
| `slogcp.LevelError` | 8 | `ERROR` |
| `slogcp.LevelCritical` | 12 | `CRITICAL` |
| `slogcp.LevelAlert` | 16 | `ALERT` |
| `slogcp.LevelEmergency` | 20 | `EMERG` |

> **Note:** When encoding JSON, `slogcp` emits Cloud Logging-approved abbreviations `D`, `I`, `N`, `W`, `E`, `C`, `A`, and `EMERG`; `DEFAULT` remains unchanged because no shorter alias exists.

## HTTP Middleware (`github.com/pjscruggs/slogcp/http`)

### Default Behaviour

`slogcphttp.Middleware(logger)` wraps `http.Handler`s to log request/response pairs in a Cloud Logging-friendly schema. By default it:

- Extracts trace context using the global OpenTelemetry propagator, falling back to `X-Cloud-Trace-Context`.
- Starts a server span when none exists (`StartSpanIfAbsent=true`).
- Emits a request-scoped logger in the context (`AttachLogger=true`) with trace attributes.
- Logs start and finish events, HTTP method/URL, status, latency, byte counts, and remote IP.
- Demotes or drops health-check chatter when enabled via the shared `chatter` engine.

Use `slogcphttp.InjectTraceContextMiddleware()` ahead of the main middleware to prioritise `X-Cloud-Trace-Context` parsing when no OpenTelemetry propagator is configured for it.

### Functional Options

| Option | Purpose |
| --- | --- |
| `WithShouldLog(func(context.Context, *http.Request) bool)` | Predicate executed after trace extraction to decide if the request should be logged. |
| `WithSkipPathSubstrings([]string)` | Drops requests whose `URL.Path` contains any listed substring. |
| `WithSuppressUnsampledBelow(slog.Leveler)` | Suppresses unsampled requests below the chosen severity (5xx responses always log). |
| `WithLogRequestHeaderKeys(keys...)` / `WithLogResponseHeaderKeys(keys...)` | Emits selected headers into structured attributes. Keys are canonicalised like `net/http`. |
| `WithRequestBodyLimit(int64)` / `WithResponseBodyLimit(int64)` | Captures up to `limit` bytes of body content for debugging. Zero disables capture. |
| `WithRecoverPanics(bool)` | Converts panics into 500 responses and logs the panic with a stack trace. |
| `WithTrustProxyHeaders(bool)` | Trusts `X-Forwarded-For` / `X-Real-IP` when extracting the client address. |
| `WithTrustProxyEvaluator(func(*http.Request) bool)` | Fine-grained proxy trust decision evaluated per request. |
| `WithContextLogger(bool)` | Enables/disables storing a request-scoped logger in the context. |
| `WithStartSpanIfAbsent(bool)` | Controls whether a server span should be started when none is present. |
| `WithTracer(trace.Tracer)` | Supplies a custom tracer used when `StartSpanIfAbsent` is true. |
| `WithChatterConfig(chatter.Config)` *(or `WithHealthCheckFilter`)* | Installs a chatter-reduction configuration shared with gRPC interceptors. |
| `WithTraceProjectID(string)` | Overrides the project ID used when formatting per-request trace attributes. |

### Environment Variables

`loadMiddlewareOptionsFromEnv` honours these variables before functional options run:

| Variable | Meaning | Default |
| --- | --- | --- |
| `SLOGCP_HTTP_SKIP_PATH_SUBSTRINGS` | Comma-separated substrings handed to `WithSkipPathSubstrings`. | none |
| `SLOGCP_HTTP_SUPPRESS_UNSAMPLED_BELOW` | Severity threshold string/integer for unsampled suppression. | none |
| `SLOGCP_HTTP_LOG_REQUEST_HEADER_KEYS` | Comma-separated request header keys to log. | none |
| `SLOGCP_HTTP_LOG_RESPONSE_HEADER_KEYS` | Comma-separated response header keys to log. | none |
| `SLOGCP_HTTP_REQUEST_BODY_LIMIT` | Byte limit for captured request bodies. | `0` (disabled) |
| `SLOGCP_HTTP_RESPONSE_BODY_LIMIT` | Byte limit for captured response bodies. | `0` (disabled) |
| `SLOGCP_HTTP_RECOVER_PANICS` | Enables panic recovery. | `false` |
| `SLOGCP_HTTP_TRUST_PROXY_HEADERS` | Trusts proxy headers when true. | `false` |
| `SLOGCP_HTTP_ATTACH_LOGGER` | Overrides whether a logger is attached to the context. | `true` |
| `SLOGCP_HTTP_START_SPAN_IF_ABSENT` | Overrides span auto-start behaviour. | `true` |
| `SLOGCP_HTTP_TRACE_PROJECT_ID` | Overrides the project used for per-request trace decoration. | empty |

Health-check and chatter controls are configured via the shared `SLOGCP_CHATTER_*` variables described later.

### Example

```go
hc := chatter.DefaultConfig()
hc.Mode = chatter.ModeOn
hc.Action = chatter.ActionMark

middleware := slogcphttp.Middleware(logger,
	slogcphttp.WithSkipPathSubstrings("/metrics"),
	slogcphttp.WithLogRequestHeaderKeys("X-Request-Id"),
	slogcphttp.WithChatterConfig(hc),
)

handler := slogcphttp.InjectTraceContextMiddleware()(middleware(myMux))
```

### HTTP Client Trace Propagation

`slogcphttp.NewTraceRoundTripper(base http.RoundTripper, opts...)` wraps a transport to inject outbound trace headers from `req.Context()`:

- `WithInjectTraceparent(bool)` – enables/disables W3C Trace Context (defaults to `true`).
- `WithInjectXCloud(bool)` – enables/disables `X-Cloud-Trace-Context` (defaults to `true`).
- `WithSkip(func(*http.Request) bool)` – predicate to skip propagation for select requests (e.g., external hosts).

Pass `nil` for `base` to wrap `http.DefaultTransport`.

## gRPC Instrumentation (`github.com/pjscruggs/slogcp/grpc`)

### Provided Interceptors

- `UnaryServerInterceptor(logger, opts...)`
- `StreamServerInterceptor(logger, opts...)`
- `NewUnaryClientInterceptor(logger, opts...)`
- `NewStreamClientInterceptor(logger, opts...)`

All interceptors share the same option set processed by `processOptions`.

### Core Behaviour

- Server interceptors extract trace context from incoming metadata using the global propagator, falling back to `x-cloud-trace-context`, and optionally start a span when none exists.
- Client interceptors propagate trace headers (`traceparent`, `tracestate`, and `x-cloud-trace-context`) unless disabled via `WithTracePropagation(false)`.
- Both sides honour the shared chatter engine for health checks and sampling when configured.
- Panic recovery, metadata logging, payload logging, and deterministic sampling are opt-in via options.

### Functional Options

| Option | Purpose |
| --- | --- |
| `WithLevels(CodeToLevel)` | Controls how gRPC status codes map to log severities (defaults to sensible info/warn/error mapping). |
| `WithShouldLog(func(context.Context, string) bool)` | Decides if a call should be logged. |
| `WithSkipPaths([]string)` | Drops calls whose method contains any provided substring. |
| `WithSamplingRate(float64)` | Deterministically samples a percentage of calls (0.0–1.0). |
| `WithLogCategory(string)` | Adds a constant `log.category` attribute to emitted records. |
| `WithPayloadLogging(bool)` | Emits request/response payloads at `DEBUG`. |
| `WithMaxPayloadSize(int)` | Truncates payload logging to the specified byte size (0 = no limit). |
| `WithMetadataLogging(bool)` | Captures request headers and response headers/trailers. |
| `WithMetadataFilter(MetadataFilterFunc)` | Filters metadata keys when metadata logging is enabled. |
| `WithPanicRecovery(bool)` | Enables/disables panic-to-error conversion (default `true`). |
| `WithAutoStackTrace(bool)` | Automatically attaches stack traces to error-level logs and recovered panics. |
| `WithContextLogger(bool)` | Controls whether a request-scoped logger is stored in the context. |
| `WithStartSpanIfAbsent(bool)` | Controls whether a server span is started when none exists. |
| `WithTracer(trace.Tracer)` | Supplies a custom tracer for span creation. |
| `WithTraceProjectID(string)` | Overrides the project used for trace-aware context loggers. |
| `WithSkipHealthChecks(bool)` | Enables a built-in config that drops standard gRPC health-check calls. |
| `WithChatterConfig(chatter.Config)` *(or `WithHealthCheckFilter`)* | Installs a custom chatter reduction configuration shared with HTTP middleware. |
| `WithTracePropagation(bool)` | Enables/disables client-side propagation of trace headers (default `true`). |

Client interceptors automatically log outgoing metadata (when enabled), response headers, trailers, payloads, and final status codes. Server interceptors add peer address, panic diagnostics, and chatter annotations.

## Chatter Reduction and Sampling (`github.com/pjscruggs/slogcp/chatter`)

Both HTTP and gRPC components consume a shared `chatter.Config`. You can pass a configuration directly via `WithChatterConfig`, or rely on environment overrides prefixed with `SLOGCP_CHATTER_` (for example `SLOGCP_CHATTER_MODE`, `SLOGCP_CHATTER_HTTP_IGNORE_PATHS`, `SLOGCP_CHATTER_GRPC_IGNORE_METHODS`, `SLOGCP_CHATTER_SAMPLE_RATE`). Invalid values are logged through the handler's internal logger but otherwise ignored so code-based configuration can take precedence.

Refer to the `chatter` package for the full list of knobs, including latency thresholds, sampling caps, proxy trust configuration, App Engine specific behaviour, and audit field names.

## Convenience Helpers

- `slogcp.Logger(ctx)` retrieves the request logger stored by middleware/interceptors.
- `slogcp.TraceAttributes(ctx, projectID)` builds Cloud Trace aware attributes for manual logger construction.
- Context logging helpers such as `slogcp.DefaultContext`, `slogcp.NoticeContext`, `slogcp.CriticalContext`, etc., provide shortcuts for emitting records at extended severities from an arbitrary context when you already have a `*slog.Logger`.

These helpers round out the configuration primitives above and align structured logs with Google Cloud's observability tools without requiring the Cloud Logging API client.
