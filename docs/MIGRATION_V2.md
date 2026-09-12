# Migrating to slogcp v2

slogcp v2 uses the Go module path `github.com/pjscruggs/slogcp/v2`.
Pub/Sub support moves into the optional `github.com/pjscruggs/slogcp-pubsub`
module. The core module no longer depends on the Pub/Sub client.

## Update imports

| Existing import | Version 2 import |
| --- | --- |
| `github.com/pjscruggs/slogcp` | `github.com/pjscruggs/slogcp/v2` |
| `github.com/pjscruggs/slogcp/slogcphttp` | `github.com/pjscruggs/slogcp/v2/slogcphttp` |
| `github.com/pjscruggs/slogcp/slogcpgrpc` | `github.com/pjscruggs/slogcp/v2/slogcpgrpc` |
| `github.com/pjscruggs/slogcp/slogcpasync` | `github.com/pjscruggs/slogcp/v2/slogcpasync` |
| `github.com/pjscruggs/slogcp/slogcppubsub` | `github.com/pjscruggs/slogcp-pubsub` |

```sh
go get github.com/pjscruggs/slogcp/v2
go mod tidy
```

Applications using Pub/Sub also install its module.

```sh
go get github.com/pjscruggs/slogcp-pubsub
```

The Pub/Sub package name stays `slogcppubsub`. Its options, propagation helpers,
and receive callbacks retain their API. Move all slogcp imports in an
application together so its handlers, options, and context helpers use the same
major version. Go treats the v1 and v2 types and context keys as separate values.

Applications using the gRPC middleware adapter also move to
`github.com/pjscruggs/slogcp-grpc-adapter/v2` so the adapter uses the same slogcp
context helpers and options as the application.

The [Pub/Sub example](https://github.com/pjscruggs/slogcp-pubsub/blob/main/.examples/pubsub/main.go)
now lives with the optional module.

## Choose Cloud Logging API delivery

Applications can keep writing structured JSON to their chosen `io.Writer`.
The optional [`slogcp-grpc`](https://github.com/pjscruggs/slogcp-grpc) module
adds delivery through the Cloud Logging gRPC API using
`slogcp.NewHandlerWithExporter`.

Supply an official Cloud Logging logger configured with the batching,
buffering, concurrency, resource, and client options your workload needs.
Application logging calls and slogcp enrichment remain available with either
delivery method. Consult the module's setup and shutdown examples before
switching transport.
