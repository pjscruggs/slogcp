# Combine gRPC enrichment with RPC event logs

## Choose the optional interoperability layer

`slogcp-grpc-adapter` lets applications use grpc-ecosystem's logging middleware
with a slogcp-backed logger. It lives in a separate Go module, so using the core
slogcp handler does not require adopting that middleware ecosystem.

Use the combined setup below when your application wants both native RPC fields
and upstream RPC event logs. The responsibilities are separate.

- The core slogcp handler formats application and middleware logs as JSON and
  reads span context for Cloud Trace correlation.
- Native `slogcpgrpc` interceptors derive RPC-scoped loggers and attach
  metadata. They do not emit access records.
- Upstream logging interceptors decide which RPC events to emit, with fields,
  durations, and levels determined by their options.
- The adapter implements the upstream `logging.Logger` interface and forwards
  events through a `*slog.Logger`, preserving each call's context.

The adapter also works on its own with upstream interceptors. You do not need
native enrichment just to route their events through slogcp.

## Prepare the application's logger and telemetry

This recipe assumes a non-nil, ungrouped `*slog.Logger` already configured with
a slogcp handler and the application's shared attributes and handler wrappers.
Use the [startup guide](../USAGE.md#create-the-handler-at-application-startup)
if that logger does not exist yet. Close its owning handler after the server and
other logging producers have stopped.

The application also owns an OTel gRPC stats handler configured with its tracer
provider, propagators, and metrics policy. Keep its exporter setup and shutdown
path. The example receives that existing handler through a `stats.Handler`
parameter. [otelgrpc.NewServerHandler][otelgrpc] constructs this interface.

Add the modules from the consuming application's module directory.

```sh
go get github.com/pjscruggs/slogcp/v2
go get github.com/pjscruggs/slogcp-grpc-adapter/v2
go get github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/logging
```

Check [`slogcp/go.mod`](../../go.mod) and the [adapter's Go
requirement][adapter-mod] for the releases your application uses. Keep existing
dependency choices that satisfy those requirements.

## Register enrichment before logging for both RPC styles

This source-file excerpt includes its required imports. `base` is the existing
logger, including shared fields such as `component`. `telemetry` is the existing
OTel stats handler. `serverOptions` carries your existing non-interceptor
options, such as server credentials and message limits. Pass the stats handler
only through `telemetry`. Merge existing interceptor chains into the shown
chains according to their ordering requirements instead of registering a second
logging interceptor.

```go
package app

import (
	"log/slog"

	"github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/logging"
	slogcpadapter "github.com/pjscruggs/slogcp-grpc-adapter/v2"
	"github.com/pjscruggs/slogcp/v2/slogcpgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/stats"
)

func newRPCServer(
	base *slog.Logger,
	telemetry stats.Handler,
	serverOptions ...grpc.ServerOption,
) *grpc.Server {
	adapted := slogcpadapter.NewLogger(nil,
		slogcpadapter.WithLogger(base),
		slogcpadapter.WithLoggerPolicy(slogcpadapter.PreferContext),
	)
	enrichment := []slogcpgrpc.Option{
		slogcpgrpc.WithLogger(base),
		slogcpgrpc.WithOTel(false),
		slogcpgrpc.WithTracePropagation(false),
	}
	events := []logging.Option{
		logging.WithLogOnEvents(logging.StartCall, logging.FinishCall),
	}
	serverOptions = append(serverOptions,
		grpc.StatsHandler(telemetry),
		grpc.ChainUnaryInterceptor(
			slogcpgrpc.UnaryServerInterceptor(enrichment...),
			logging.UnaryServerInterceptor(adapted, events...),
		),
		grpc.ChainStreamInterceptor(
			slogcpgrpc.StreamServerInterceptor(enrichment...),
			logging.StreamServerInterceptor(adapted, events...),
		),
	)
	return grpc.NewServer(serverOptions...)
}
```

Call this helper during server construction, register your generated services on
the returned server, and use your existing listener and `Serve` lifecycle. It is
not a complete program or a replacement for your service implementations.

The first interceptor in each chain is outermost. Native enrichment installs the
contextual logger before the logging interceptor captures its event context. For
streaming RPCs, it passes a wrapped stream whose `Context` returns that context.
Application handlers should use `slogcp.Logger(ctx).InfoContext(ctx, ...)` and
obtain `ctx` from `stream.Context()` in streaming handlers. This call needs an
import of `github.com/pjscruggs/slogcp/v2` in the service implementation.

`WithLogger(base)` preserves the existing logger's attributes and pipeline.
Selection is fixed unless `PreferContext` is enabled. With `PreferContext`, each
event uses the logger stored in its context or falls back to `base` if none is
present. Selection uses the whole logger, including its destination and filter.
It does not merge attributes from unrelated loggers or retry filtered events
through the fallback. Here, enrichment derives from `base`, so both paths keep
the shared policy and fields.

A logger added to a new child context inside a service handler does not replace
the context already retained for upstream completion events. Add attributes
needed on those events in an outer interceptor or native attribute enricher. See
the [adapter's selection and ordering guidance][adapter-selection].

## Keep completion fields and telemetry ownership clear

The example explicitly chooses start and finish events without payload logging.
For terminal outcomes, use upstream `grpc.code` and `grpc.time_ms` from the
`finished call` event. Native `grpc.status_code` and `rpc.duration` are not a
substitute for these completion fields. The outer native interceptor finalizes
its request state after the inner logging interceptor returns, and bound fields
may have been resolved earlier. In particular, a failing RPC's native status
field can still say `OK` in the upstream completion log.

Continue configuring event levels and extra fields with upstream options such as
`logging.WithLevels` and `logging.WithFieldsFromContext`. A failure does not
necessarily log at ERROR under upstream's default status-to-level mapping. See
the [upstream options and event contracts][upstream].

The individual native interceptor constructors do not install OTel stats
handlers. This recipe uses the application's existing stats handler once and
disables native metadata extraction so the existing instrumentation owns
propagation and trust decisions. The native option bundles `ServerOptions` and
`DialOptions` can install stats handlers, so do not add them on top of this
setup. See the [native package guide](../../slogcpgrpc/README.md).

Correlation reads the span context at the logging call. Propagation carries
context across RPC boundaries. Recording and export require the application's
[OTel provider and export configuration][otel-setup]. The adapter configures
none of those components. Disabling enrichment's propagation does not prevent
slogcp from correlating logs with a span the existing stats handler supplied.

## Verify real calls before deploying

Run `go mod tidy` and `go test ./...` in the consumer module. Exercise the
actual server through local unary and streaming clients, using `bufconn` or a
loopback listener. Use an in-memory log writer and span exporter so the test
needs no Cloud credentials.

Verify successful calls and a handler failure such as `codes.Internal`. For
calls that exchange messages, expect `started call` and `finished call` events.
Assert the final `grpc.code`, the configured severity, and a duration field.
Check the shared logger attributes and native `rpc.system`, `rpc.service`, and
`rpc.method` fields on both events. Compare log trace/span IDs to the server
span, and assert that there is only one server span per RPC. Ensure streaming
clients read to EOF or the terminal status so completion behavior is exercised.

As a selection check, omit `PreferContext` and verify that upstream events still
retain `base` attributes but no longer inherit native fields. As an adapter-only
check, remove the two native interceptors and use fixed selection. The upstream
RPC event logs should remain. Removing the upstream logging interceptors instead
leaves enrichment and application logging but removes those automatic events.

Local tests establish logging and instrumentation behavior, not Cloud delivery.
After deployment, separately check ingestion and trace retention. The handler
writes JSON rather than calling the Cloud Logging API. Inspect promoted `trace`
and `spanId` fields at the LogEntry level, following [Cloud Logging's structured
logging guidance][structured].

[adapter-mod]:
  https://github.com/pjscruggs/slogcp-grpc-adapter/blob/main/go.mod
[adapter-selection]:
  https://github.com/pjscruggs/slogcp-grpc-adapter#selecting-a-request-scoped-logger
[upstream]:
  https://pkg.go.dev/github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/logging
[otelgrpc]:
  https://pkg.go.dev/go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc#NewServerHandler
[otel-setup]: https://opentelemetry.io/docs/languages/go/instrumentation/
[structured]: https://docs.cloud.google.com/logging/docs/structured-logging
