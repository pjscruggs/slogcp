# slogcppubsub

`github.com/pjscruggs/slogcp/slogcppubsub` provides Pub/Sub helpers for slogcp:

- `Inject` / `Extract` for trace context propagation via `pubsub.Message.Attributes`.
- `WrapReceiveHandler` to derive a per-message `*slog.Logger`, attach it to the handler context (so `slogcp.Logger(ctx)` works), and optionally start an application-level consumer span.

It enriches application logs; it does not emit message receive logs by itself.

## Install

```bash
go get github.com/pjscruggs/slogcp
```

Import: `github.com/pjscruggs/slogcp/slogcppubsub`

## Publisher (inject)

```go
msg := &pubsub.Message{Data: []byte("hello")}
slogcppubsub.Inject(ctx, msg) // injects trace context

res := topic.Publish(ctx, msg)
_ = res
```

By default, slogcppubsub injects W3C trace context only. To also propagate
baggage, enable it explicitly (and avoid propagating sensitive/high-cardinality
data because Pub/Sub attributes are size-limited):

```go
slogcppubsub.Inject(ctx, msg, slogcppubsub.WithBaggagePropagation(true))
```

To also write the Go client's `googclient_`-prefixed keys:

```go
slogcppubsub.Inject(ctx, msg, slogcppubsub.WithGoogClientInjection(true))
```

## Subscriber (pull)

```go
handler, err := slogcp.NewHandler(os.Stdout)
if err != nil {
	log.Fatal(err)
}
logger := slog.New(handler)

err = sub.Receive(ctx, slogcppubsub.WrapReceiveHandler(
	func(ctx context.Context, msg *pubsub.Message) {
		slogcp.Logger(ctx).Info("processing message", "id", msg.ID)
		msg.Ack()
	},
	slogcppubsub.WithLogger(logger),
	slogcppubsub.WithSubscription(sub),
	// Optional: accept/extract googclient_traceparent when present.
	slogcppubsub.WithGoogClientExtraction(true),
))
```

## Trust boundary (public endpoint)

If you do not trust producer trace IDs, enable public endpoint mode to start a
new root trace and link to the extracted remote context instead of parenting:

```go
wrapped := slogcppubsub.WrapReceiveHandler(handler, slogcppubsub.WithPublicEndpoint(true))
```

When public endpoint mode is enabled and an upstream trace context is present,
derived loggers also include `pubsub.remote.traceparent` for debugging without
using it for Cloud Logging trace correlation.

To restore "only inject when a span is present" behavior:

```go
slogcppubsub.Inject(ctx, msg, slogcppubsub.WithInjectOnlyIfSpanPresent(true))
```

When public endpoint mode is enabled and `WithOTel(false)` is used, logs do not
correlate to extracted remote trace context by default. If you explicitly want
that behavior (or set `SLOGCP_TRUST_REMOTE_TRACE=true`):

```go
wrapped := slogcppubsub.WrapReceiveHandler(
	handler,
	slogcppubsub.WithPublicEndpoint(true),
	slogcppubsub.WithOTel(false),
	slogcppubsub.WithRemoteTrace(true),
)
```

## Logger Cardinality

Derived loggers avoid high-cardinality fields by default. Enable them when
needed for debugging:

```go
wrapped := slogcppubsub.WrapReceiveHandler(
	handler,
	slogcppubsub.WithLogMessageID(true),
	slogcppubsub.WithLogOrderingKey(true),
	slogcppubsub.WithLogPublishTime(true),
)
```

## Push Subscriptions

For push subscriptions delivering to HTTP endpoints, Pub/Sub does not send
message attributes as HTTP headers unless you configure payload unwrapping and
metadata writing. To preserve end-to-end trace continuity, inject standard W3C
keys (`traceparent`/`tracestate`) into message attributes on publish and ensure
your push configuration forwards them as headers so your HTTP middleware can
extract them.

## More

- Configuration reference: `../docs/CONFIGURATION.md` (see "Pub/Sub Integration")
- Runnable example: `../.examples/pubsub`
