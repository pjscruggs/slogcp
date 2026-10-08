# Test log correlation and exported spans separately

Use this executable local recipe to verify an HTTP trace, request child spans,
and detached work under all three logger selection styles. It uses a Cloud
Run-style `X-Cloud-Trace-Context` header, an actual slogcp JSON handler, and an
OpenTelemetry in-memory exporter. It needs no cloud credentials or deployment.

From the repository root:

```sh
cd .examples/trace-correlation
go test -race -count=1 -v ./...
```

The [application helper](../../.examples/trace-correlation/job.go) and
[regression tests](../../.examples/trace-correlation/job_test.go) use the
checked-out slogcp source through their module's local replacement. They are a
complete test fixture that can be adapted to a consuming application.

## Assert identities and logger fields

`TestCorrelation` runs sampled and unsampled requests for each of:

- Global logging through `slog.InfoContext`.
- An explicit logger's `InfoContext` method.
- Context lookup through `slogcp.Logger(ctx).InfoContext(ctx, ...)`.

It installs the global SDK before composing HTTP middleware and supplies a W3C
propagator while exercising the legacy Cloud Trace header fallback. Processing
starts only after the HTTP handler returns, the server span ends, and the
request is canceled. A request-only context value and baggage must be absent
from the job. The captured trace-project override must remain available.

JSON is decoded into entries. Each entry's trace, span ID, and sampling flag are
compared with the active span context recorded at that log call. Shared base
fields and the explicit `job_id` must be present. Only contextual request logs
inherit the middleware's HTTP fields and request-only logger attribute. Detached
logs must not inherit those fields, and global or injected loggers must retain
their own selected pipeline.

## Assert exported relationships independently

For sampled requests, expect exactly these relationships:

| Span | Parent |
| --- | --- |
| HTTP server | Remote span from the inbound header |
| Request child | HTTP server |
| Detached job | HTTP server, even though it has ended |
| Job child | Detached job |

All four spans share the incoming trace ID and have distinct local span IDs.
The test also rejects unexpected links. A deterministic parent-based sampler
ensures unsampled incoming requests export zero spans while their logs still
carry correlation fields and `trace_sampled=false`.

`TestJobLifetime` covers cancellation before processing, an already expired
deadline, cancellation during processing, failure status, and an invalid origin
starting a root span. The [shared-work recipe](shared-work-consumption.md) has a
separate executable test for new roots and consumer links.

Global logger/provider tests run serially and restore process state. These local
checks establish source behavior, JSON identities, and SDK export relationships.
They do not establish Cloud Logging ingestion, successful remote trace export,
or trace retention. After application deployment, verify those paths separately.
A trace field in a collected log is insufficient evidence that spans reached the
trace backend.
