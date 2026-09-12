# Add message fields to an already instrumented Pub/Sub subscriber

Use this recipe when the application's Pub/Sub client or callback wrapper
already supplies the span you want application logs to use. Keep that client's
tracing configuration, propagators, sampling, and export pipeline. Google
documents the client tracing setup in its [Pub/Sub OpenTelemetry
guide][tracing].

The core slogcp handler can correlate logs with that callback context directly.
Add `slogcppubsub.WrapReceiveHandler` when you also want message-scoped fields.
This recipe uses the Pub/Sub v2 Go client; see the [startup guide](../USAGE.md)
for constructing the slogcp-backed base logger and the [module's Go
requirement](../../go.mod).

## Preserve the callback's span

This source-file excerpt includes its imports. Pass your configured, ungrouped
base logger, the trace project ID used by its handler, your subscription ID, and
the existing processing function. The processing function must be safe for
concurrent callbacks and tolerate redelivery.

```go
package app

import (
	"context"
	"log/slog"

	"cloud.google.com/go/pubsub/v2"
	"github.com/pjscruggs/slogcp"
	"github.com/pjscruggs/slogcp/slogcppubsub"
)

func messageHandler(
	base *slog.Logger,
	projectID, subscriptionID string,
	process func(context.Context, []byte) error,
) func(context.Context, *pubsub.Message) {
	return slogcppubsub.WrapReceiveHandler(
		func(ctx context.Context, msg *pubsub.Message) {
			logger := slogcp.Logger(ctx)
			if err := process(ctx, msg.Data); err != nil {
				logger.ErrorContext(ctx, "message processing failed",
					slog.Any("error", err))
				msg.Nack()
				return
			}
			logger.InfoContext(ctx, "message processed")
			msg.Ack()
		},
		slogcppubsub.WithLogger(base),
		slogcppubsub.WithProjectID(projectID),
		slogcppubsub.WithSubscriptionID(subscriptionID),
		slogcppubsub.WithOTel(false),
		slogcppubsub.WithTracePropagation(false),
		slogcppubsub.WithLogMessageID(true),
	)
}
```

Pass the returned callback to your existing subscriber's `Receive` call. If an
application wrapper creates the processing span, place this callback inside that
wrapper so it receives the span context. Retain the existing handling of
`Receive` errors and client shutdown.

`WithOTel(false)` prevents an extra slogcppubsub consumer span.
`WithTracePropagation(false)` prevents message attributes from replacing the
span context supplied by the existing instrumentation. Disabling only span
creation still permits extraction. `SpanStrategyAuto` alone is also insufficient
to promise preservation: extraction happens before the strategy checks the
current span. See the [receive implementation](../../slogcppubsub/receive.go)
and [propagation implementation](../../slogcppubsub/propagation.go).

The contextual logger retains base attributes and adds fields such as
`messaging.system`, `messaging.destination.name`, and `messaging.message.id`.
Message IDs are explicitly enabled here to help investigate redelivery; omit
that option if your application does not need them. The helper does not log the
message body. It also emits no automatic receive or completion record: the
callback above owns those log calls.

The success record means processing returned successfully, before `Ack` was
called. It does not confirm a server-accepted acknowledgment. Adapt the shown
Nack-on-error policy to your retry and dead-letter handling. Ack/Nack must
happen inside the callback; the client's [Receive contract][receive] also
describes concurrency and shutdown behavior.

## Verify context preservation and completion behavior

In a local test, supply a recording tracer provider and an in-memory log writer.
Create a local callback span and give the message a valid `traceparent`
attribute from a *different* trace. Invoke the returned callback and verify:

- Processing and logging use the callback span, not the message attribute's
  trace. No additional wrapper span is exported.
- The JSON retains the base fields and contains the subscription and message
  IDs. With the trace project configured, its Cloud trace and span fields match
  the callback span.
- Success emits one INFO completion record; a processing error emits one ERROR
  record. A child span created by processing is used by logs that pass its
  context, even when they reuse the message logger.

Directly invoking the callback with a synthetic message tests enrichment and
processing, but does not prove acknowledgments reached Pub/Sub. Exercise actual
success, redelivery, and shutdown against your application's integration-test
subscription separately.

If the supplied context has no valid span, this configuration creates none and
does not recover one from message attributes. For a subscriber that needs
slogcppubsub to own extraction and consumer spans, follow the [Pub/Sub package
guide](../../slogcppubsub/README.md) instead, including its trust-boundary
options. HTTP push delivery needs an HTTP integration, as described in the
[usage guide](../USAGE.md#pubsub-and-other-client-libraries).

[tracing]: https://docs.cloud.google.com/pubsub/docs/open-telemetry-tracing
[receive]: https://pkg.go.dev/cloud.google.com/go/pubsub/v2#Subscriber.Receive
