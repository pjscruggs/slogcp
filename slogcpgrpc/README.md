# slogcpgrpc

`github.com/pjscruggs/slogcp/slogcpgrpc` provides gRPC interceptors for slogcp:

- Server + client unary/stream interceptors that derive a per-RPC `*slog.Logger` and attach it to the RPC `context.Context` (so `slogcp.Logger(ctx)` works in handlers).
- A `RequestInfo` snapshot (method/service/kind/status/latency, plus optional peer + payload sizes) available via `InfoFromContext`.
- Optional OpenTelemetry instrumentation via `otelgrpc` StatsHandlers (spans + metrics).

It enriches application logs; it does not emit access logs by itself.

## Install

```bash
go get github.com/pjscruggs/slogcp
```

Import: `github.com/pjscruggs/slogcp/slogcpgrpc`

## Server

```go
handler, err := slogcp.NewHandler(os.Stdout)
if err != nil {
	log.Fatal(err)
}
logger := slog.New(handler)

server := grpc.NewServer(
	slogcpgrpc.ServerOptions(
		slogcpgrpc.WithLogger(logger),
		// Optional: if you want to override runtime detection
		// slogcpgrpc.WithProjectID("proj-123"),
	)...,
)
pb.RegisterGreeterServer(server, &greeter{})
```

In handlers:

```go
func (s *greeter) SayHello(ctx context.Context, req *pb.HelloRequest) (*pb.HelloReply, error) {
	slogcp.Logger(ctx).Info("handling request", "name", req.Name)

	if info, ok := slogcpgrpc.InfoFromContext(ctx); ok {
		_ = info // inspect status, latency, sizes, peer, etc.
	}
	return &pb.HelloReply{Message: "hi"}, nil
}
```

For streaming RPCs, the server stream is wrapped so `stream.Context()` returns the derived context.

## Client

```go
conn, err := grpc.NewClient(
	target,
	append(
		[]grpc.DialOption{grpc.WithTransportCredentials(creds)},
		slogcpgrpc.DialOptions(
			slogcpgrpc.WithLogger(logger),
		)...,
	)...,
)
if err != nil {
	log.Fatal(err)
}
defer conn.Close()

client := pb.NewGreeterClient(conn)
resp, err := client.SayHello(ctx, &pb.HelloRequest{Name: "world"})
_ = resp
_ = err
```

## OpenTelemetry and trace propagation

By default, `slogcpgrpc`:

- installs `otelgrpc` StatsHandlers (spans + metrics) via `ServerOptions`/`DialOptions`, and
- reads/writes trace context via the configured (or global) OpenTelemetry propagator.

Options:

- `WithOTel(false)` disables `otelgrpc` StatsHandlers (no spans/metrics are created by this package).
- `WithTracePropagation(false)` disables reading/writing trace context metadata. When `WithOTel(true)`, spans are still created, but they start new root traces instead of continuing incoming traces.
- `WithPropagators(...)` supplies a specific propagator for metadata extraction/injection.
- `WithPublicEndpoint(true)` configures `otelgrpc` to *link* to incoming trace context instead of using it as the parent.
- `WithLegacyXCloudInjection(true)` synthesizes legacy `x-cloud-trace-context` on outbound RPCs in addition to standard propagation.

For log↔trace correlation in Cloud Logging, slogcp emits `logging.googleapis.com/trace`, `logging.googleapis.com/spanId`, and `logging.googleapis.com/trace_sampled` when a span is present in the context.

## Request metadata fields

The derived logger includes trace correlation fields (when available) plus RPC metadata:

- `rpc.system`, `rpc.service`, `rpc.method`, `grpc.type`
- `grpc.status_code`, `rpc.duration`
- `net.peer.ip` (enabled by default; `WithPeerInfo(false)` disables)
- `rpc.request_size`, `rpc.response_size`, `rpc.request_count`, `rpc.response_count` (enabled by default; `WithPayloadSizes(false)` disables)

## Custom attributes

- `WithAttrEnricher(func(context.Context, *RequestInfo) []slog.Attr)` appends custom fields to the derived logger.
- `WithAttrTransformer(func(context.Context, []slog.Attr, *RequestInfo) []slog.Attr)` can redact or reshape derived attributes before they are applied.
- `WithSpanAttributes(...)` and `WithFilter(...)` mirror `otelgrpc` configuration knobs and only apply when OpenTelemetry instrumentation is enabled.

## More

- Configuration reference: `../docs/CONFIGURATION.md` (see "gRPC Integration")
- Runnable example: `../.examples/grpc`
