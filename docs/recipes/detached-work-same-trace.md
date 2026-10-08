# Continue a request trace in detached work

Use this pattern when queued work belongs to the initiating trace but must
survive the request ending or being canceled. The application owns its queue,
worker shutdown, immutable job data, and recording OpenTelemetry provider.
For independent work with a separate sampling decision, keep the
[new-trace-with-origin-link recipe](background-job-tracing.md).

## Capture identity, not the request context

Capture only the initiating `trace.SpanContext`, the optional slogcp
trace-project override, and approved job data. Never retain a request context or
automatically reuse its logger. Request values can include pinned database
connections and request-only attributes. [`context.WithoutCancel`][without]
removes cancellation but retains values, so it does not provide this isolation.
Do not copy baggage automatically.

The following source-file excerpt matches the application-local helper in the
[executable example](../../.examples/trace-correlation/job.go). It adds no new
slogcp API. Supply a live worker context, a non-nil base logger, a tracer from
your recording provider, and a cooperative processing function.

```go
package correlation

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/pjscruggs/slogcp/v2"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type job struct {
	id      string
	origin  trace.SpanContext
	project string
}

func captureJob(ctx context.Context, id string) job {
	project, _ := slogcp.TraceProjectIDFromContext(ctx)
	return job{id: id, origin: trace.SpanContextFromContext(ctx), project: project}
}

func runJob(workerCtx context.Context, base *slog.Logger, tracer trace.Tracer,
	queued job, timeout time.Duration, process func(context.Context) error,
) error {
	ctx, cancel := context.WithTimeout(workerCtx, timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("job lifetime: %w", err)
	}
	if queued.origin.IsValid() {
		ctx = trace.ContextWithSpanContext(ctx, queued.origin)
	}
	if queued.project != "" {
		ctx = slogcp.ContextWithTraceProjectID(ctx, queued.project)
	}
	logger := base.With("job_id", queued.id)
	ctx = slogcp.ContextWithLogger(ctx, logger)
	ctx, span := tracer.Start(ctx, "process detached job")
	defer span.End()
	logger.InfoContext(ctx, "job started")
	if err := process(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "job processing failed")
		logger.ErrorContext(ctx, "job failed", slog.Any("error", err))
		return err
	}
	logger.InfoContext(ctx, "job completed")
	return nil
}
```

Call `captureJob` while the initiating span is active, enqueue the resulting
identity and owned payload, then call `runJob` after dequeueing. The request
span can end before the job starts. The job span has a new span ID, the same
trace ID, and the initiating span as parent. Worker values are inherited from
the supplied worker context; request values are not. Use a dedicated worker
context without an unrelated active span when an origin may be absent.

An invalid origin starts a new trace under that worker context. A parent-based
sampler normally preserves an unsampled initiating trace. A trace-project
override selects the project used for log correlation; it does not configure
the span exporter. An explicit handler trace-project setting takes precedence
over a context override, as described in [configuration](../CONFIGURATION.md).

## Keep the application's logger selection style

All three styles remain supported. Supply `slog.Default()` as `base` for a
global-style application or your injected logger for explicit logging. The
helper explicitly attaches its derived job logger for code using context lookup.

| Processing call | Logger selected |
| --- | --- |
| `slog.InfoContext(ctx, "processing", "job_id", queued.id)` | Current global default |
| `base.InfoContext(ctx, "processing", "job_id", queued.id)` | Explicit base |
| `slogcp.Logger(ctx).InfoContext(ctx, "processing")` | Attached job logger with `job_id` |

Passing a context does not make global or injected loggers inherit fields from
the attached job logger. Add approved job fields explicitly or derive the logger
you intend to use. Preserve the current child-span context at every log call;
logger selection and trace selection are separate operations.

Worker cancellation and the job deadline bound cooperative processing. The
processing function must observe cancellation or pass the context to operations
that do. Finish workers before closing the log handler and shutting down the
provider. This pattern supplies no queue durability, retry, or deployment drain
guarantee. Crossing process boundaries requires transport-specific propagation.

Run the [correlation regression recipe](trace-correlation-regression.md) to verify
parentage, cancellation isolation, logger fields, and unsampled requests.

[without]: https://pkg.go.dev/context#WithoutCancel
