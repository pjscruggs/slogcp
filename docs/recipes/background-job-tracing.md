# Correlate queued background work with its originating request

Use this recipe for an application that already owns an in-process job queue,
worker lifecycle, and OpenTelemetry provider. A queued job can outlive the
request that submitted it. Capture the originating span's identity, then run the
job under the worker's cancellation context with its own trace and a link to
that origin. OpenTelemetry documents [span links][links] for relationships
between causally connected operations, including long-running asynchronous work.

This is a choice for independent jobs. Work that remains part of a request and
must stop with it can use a child span and the request context directly.

## Carry identity across the queue, and use the worker's lifetime

This source-file excerpt includes its imports. Call `makeJob` at submission,
enqueue its result through your existing queue, and call `runJob` synchronously
from a worker after dequeueing. `workerCtx` is canceled by worker shutdown;
`tracer` comes from the application's recording provider. Supply your configured
base logger and processing function. The payload string stands in for your
application's owned, immutable job data.

```go
package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/pjscruggs/slogcp/v2"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type Job struct {
	ID      string
	Payload string
	Origin  trace.SpanContext
}

func makeJob(ctx context.Context, id, payload string) Job {
	return Job{ID: id, Payload: payload, Origin: trace.SpanContextFromContext(ctx)}
}

func runJob(
	workerCtx context.Context,
	base *slog.Logger,
	tracer trace.Tracer,
	job Job,
	process func(context.Context, string) error,
) error {
	ctx, cancel := context.WithTimeout(workerCtx, 30*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}

	opts := []trace.SpanStartOption{trace.WithNewRoot()}
	if job.Origin.IsValid() {
		opts = append(opts, trace.WithLinks(trace.Link{SpanContext: job.Origin}))
	}
	ctx, span := tracer.Start(ctx, "process queued job", opts...)
	defer span.End()

	logger := base.With("job_id", job.ID)
	ctx = slogcp.ContextWithLogger(ctx, logger)
	logger.InfoContext(ctx, "job started")
	if err := process(ctx, job.Payload); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "job processing failed")
		logger.ErrorContext(ctx, "job failed", slog.Any("error", err))
		return err
	}
	logger.InfoContext(ctx, "job completed")
	return nil
}
```

The job stores neither the request context nor its request-scoped logger. Shared
application attributes and policies come from `base`; only approved job fields
are added. Code inside `process` can select `slogcp.Logger(ctx)` and pass its
current context to each log call, including contexts containing child spans.

The worker context preserves shutdown cancellation, while the timeout bounds
cooperative processing. As with Go's [context cancellation][context], the
processing function must observe `ctx.Done()` or pass the context to operations
that do. The timeout cannot forcibly stop code that ignores cancellation. Choose
the duration according to your workload.

The origin link survives the originating span ending. The job's logs correlate
to the job's new trace; they will not be grouped under the originating request
trace in Cloud Logging. Give submission logs the same `job_id` if operators need
a log search that crosses this boundary. Trace links are available through your
trace backend when the relevant spans are exported and retained. A new root also
has its own sampling decision under your configured sampler.

Both logging and span error reporting are explicit. OTel's `RecordError` does
not set the span status by itself, so the failure path also calls `SetStatus`,
following its [Go instrumentation guidance][instrumentation].

## Verify cancellation and trace relationships

Use an in-memory span exporter and log writer. Start a request span, call
`makeJob`, end that span, and cancel its context before running the queued job
with a live worker context. Assert that processing still runs, the job span is a
new root with exactly one link to the origin, and both job logs carry the job
span's Cloud trace/span IDs plus `job_id` and base fields.

Also test an invalid origin (no link), processing failure (ERROR log and error
span status), and worker cancellation before dequeueing (processing is skipped).
Cancel the worker during processing to verify cancellation reaches it. Wait for
workers to finish before closing the slogcp handler and shutting down the OTel
provider; see the [shutdown
guide](../USAGE.md#manage-configuration-and-shutdown).

This helper does not make an in-process queue durable. Keep your existing
enqueue, retry, and deployment-lifecycle guarantees. For a queue crossing a
process boundary, serialize trace context with your transport's propagator; the
[Pub/Sub integration](../../slogcppubsub/README.md) covers message attributes.

[links]: https://opentelemetry.io/docs/concepts/signals/traces/#span-links
[context]: https://pkg.go.dev/context#WithTimeout
[instrumentation]: https://opentelemetry.io/docs/languages/go/instrumentation/#record-errors
