# Write logs through the Cloud Logging gRPC API

Use the optional `slogcp-grpc` module when you want to tune the Cloud Logging client's batching, buffering, and concurrent writes while keeping your `slog` calls and `slogcp` enrichment. The application can trade additional CPU and memory for delivery throughput that suits its workload.

Create Google's official `logging.Client` and configure its logger directly. All client and logger options remain available. Set `Client.OnError` before creating a logger so asynchronous delivery failures reach your error handler.

This complete program uses Application Default Credentials and reads its destination from `GOOGLE_CLOUD_PROJECT`. The caller needs permission to write logs in that project. The [runnable example](../../.examples/cloud-logging-grpc) includes a local gRPC test and a [module manifest](../../.examples/cloud-logging-grpc/go.mod) with its dependencies.

```go
// Command cloud-logging-grpc sends a structured log through the Cloud Logging API.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"os"
	"time"

	"cloud.google.com/go/logging"

	"github.com/pjscruggs/slogcp/v2"

	slogcpgrpc "github.com/pjscruggs/slogcp-grpc"
)

// main writes the example log and reports setup or delivery errors.
func main() {
	if err := run(context.Background()); err != nil {
		log.Fatal(err)
	}
}

// run reads the destination project and owns the logging client's lifetime.
func run(ctx context.Context) (result error) {
	projectID := os.Getenv("GOOGLE_CLOUD_PROJECT")
	if projectID == "" {
		return errors.New("set GOOGLE_CLOUD_PROJECT to your logging project")
	}
	client, err := logging.NewClient(ctx, projectID)
	if err != nil {
		return fmt.Errorf("create logging client %w", err)
	}
	defer func() { result = errors.Join(result, client.Close()) }()
	client.OnError = func(err error) { fmt.Fprintln(os.Stderr, err) }
	return emit(ctx, client, projectID)
}

// emit configures buffered delivery and drains the handler and exporter.
func emit(ctx context.Context, client *logging.Client, projectID string) error {
	cloudLogger := client.Logger("application",
		logging.DelayThreshold(100*time.Millisecond),
		logging.EntryCountThreshold(1000),
		logging.ConcurrentWriteLimit(4),
		logging.BufferedByteLimit(32<<20),
		logging.ContextFunc(func() (context.Context, func()) {
			return context.WithTimeout(context.Background(), 10*time.Second)
		}),
	)
	exporter, err := slogcpgrpc.NewExporter(cloudLogger)
	if err != nil {
		return fmt.Errorf("create exporter %w", err)
	}
	handler, err := slogcp.NewHandlerWithExporter(exporter, slogcp.WithTraceProjectID(projectID))
	if err != nil {
		return fmt.Errorf("create handler %w", err)
	}
	logger := slog.New(handler)
	logger.InfoContext(ctx, "service ready", "transport", "grpc")
	return errors.Join(handler.Close(), exporter.Flush())
}
```

The client owns its delivery buffers. After logging producers have stopped, close the handler, flush the exporter, and close the client. Check all returned errors. The example preserves errors from each shutdown step.

If you add `slogcp.WithAsync`, finish draining or aborting the handler before closing the client. With `slogcp.WithCloseTimeoutPolicy(slogcp.CloseTimeoutReturn)`, a `Close` timeout leaves workers running. Keep the client open until a later `Shutdown` succeeds or `Abort(context.Background())` finishes. A timed-out `Abort` can also leave workers running.

The sample bounds background RPCs through `logging.ContextFunc`. Buffered writes use this context even when the context supplied to a log call has ended. Trace correlation still comes from the record context.

`slogcpgrpc.WithSynchronous()` makes each export wait for `logging.Logger.LogSync` using the record context. Direct calls to `Handler.Handle` receive delivery errors when the handler has no async queue. With `slogcp.WithAsync`, `Handle` returns after enqueue and exporter errors go to the writer configured by `slogcpasync.WithErrorWriter`. Ordinary `slog.Logger` methods discard handler errors.

`slogcpgrpc.WithEntryMutator` lets applications set per-entry monitored resources, insert IDs, operations, or other writable Cloud Logging entry fields.

The [client options](https://pkg.go.dev/cloud.google.com/go/logging#LoggerOption) describe batching thresholds, memory limits, resource detection, and partial batch success. Measure the intended runtime before selecting limits. The [module benchmarks](https://github.com/pjscruggs/slogcp-grpc/blob/main/exporter_bench_test.go) compare buffered delivery through the official client and the exporter.
