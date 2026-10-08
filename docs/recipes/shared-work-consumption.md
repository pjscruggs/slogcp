# Follow work shared by several requests

Use this pattern when one request initiates work and another request consumes
the same result. The application decides whether work is shared and owns the
job registry, result lifetime, concurrency, and retries. slogcp cannot infer
that relationship.

Give the work a stable `job_id`. Capture the initiating span identity separately
as `origin_trace_id` and `origin_span_id`. Choose whether the job continues the
[origin trace](detached-work-same-trace.md) or has its own
[root with an origin link](background-job-tracing.md). Expose its span context
to consumers while retaining only approved, immutable metadata.

The following source-file excerpt links a consumer operation to the job while
keeping it parented to the consuming request. Supply a non-nil selected logger
and a tracer from your recording provider.

```go
package correlation

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

func consumeShared(ctx context.Context, logger *slog.Logger, tracer trace.Tracer,
	jobID string, origin, work trace.SpanContext,
	consume func(context.Context) error,
) error {
	var opts []trace.SpanStartOption
	if work.IsValid() {
		opts = append(opts, trace.WithLinks(trace.Link{SpanContext: work}))
	}
	ctx, span := tracer.Start(ctx, "consume shared job", opts...)
	defer span.End()
	logger = logger.With("job_id", jobID)
	if origin.IsValid() {
		logger = logger.With("origin_trace_id", origin.TraceID().String(),
			"origin_span_id", origin.SpanID().String())
	}
	logger.InfoContext(ctx, "shared result consumed")
	return consume(ctx)
}
```

Use the same `job_id` and origin fields on submission and job logs. The
consumer's Cloud trace/span fields describe its own active operation; origin
fields are ordinary application attributes and must not overwrite correlation
fields. A second request can consume the result without adopting the first
request's trace. Span links preserve the relationship when relevant spans are
exported and retained; the shared job ID supports log searches across traces.

Global-style code can emit `slog.InfoContext(ctx, ...)` with those explicit
fields. Context-style code can attach the deliberately selected derived logger
and call `slogcp.Logger(ctx).InfoContext(ctx, ...)`. Neither style automatically
inherits another request's logger.

The executable [`TestSharedWork`](../../.examples/trace-correlation/shared_test.go)
asserts an independent job root linked to its origin, a consumer child in a
different trace linked to the job, and JSON fields for the job and consumer.
Invalid identities omit links and origin fields. This recipe adds no scheduling,
retry budget, cache, or transport timing behavior.
