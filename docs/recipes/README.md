# Application integration recipes

Use these recipes when adding slogcp to a separate Go application. Start with
what the application already owns and the behavior you want to add.

- [Keep existing HTTP OpenTelemetry instrumentation](http-existing-otel.md) when
  you want Cloud Logging-compatible output from slog calls. Add optional
  request-scoped fields while keeping your current tracing configuration.
- [Combine gRPC enrichment and RPC event
  logs](grpc-enrichment-and-access-logs.md) when you want native slogcpgrpc
  fields on events emitted by go-grpc-middleware. The optional adapter also
  works without native enrichment.
- [Keep existing Pub/Sub OpenTelemetry instrumentation](pubsub-existing-otel.md)
  when you want message fields and completion logs while preserving the span
  already supplied to a pull subscriber's callback.
- [Correlate queued background jobs](background-job-tracing.md) when work can
  outlive its originating request and needs its own cancellation lifetime and
  trace linked to that request.
- [Redact sensitive structured fields](redact-sensitive-fields.md) when domain
  objects and request attributes need an explicit logging representation and a
  policy for sensitive keys.
- [Change log levels at runtime](runtime-log-level.md) when an existing
  configuration control should update base and derived loggers without
  rebuilding their handlers.

If you only need structured output and trace correlation, the core handler may
be enough. It writes JSON to an `io.Writer`. Your deployment must collect that
output, and your application still owns trace recording and export.

Each recipe states its prerequisites, includes source-file excerpts with
imports, and describes checks to run in the consuming application. References
link to the relevant upstream contracts; local JSON and span tests do not prove
Cloud ingestion, acknowledgment delivery, or trace retention.

For logger selection, startup, and shutdown, use the [usage guide](../USAGE.md).
For options and environment variables, use the [configuration
reference](../CONFIGURATION.md). Check the Go requirement in
[`go.mod`](../../go.mod) for the release your application uses.
