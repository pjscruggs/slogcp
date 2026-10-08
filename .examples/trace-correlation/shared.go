// Copyright 2025-2026 Patrick J. Scruggs
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package correlation

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// consumeShared keeps consumer parentage and adds a link to the shared job.
func consumeShared(ctx context.Context, logger *slog.Logger, tracer trace.Tracer, jobID string, origin, work trace.SpanContext, consume func(context.Context) error) error {
	var opts []trace.SpanStartOption
	if work.IsValid() {
		opts = append(opts, trace.WithLinks(trace.Link{SpanContext: work}))
	}
	ctx, span := tracer.Start(ctx, "consume shared job", opts...)
	defer span.End()
	logger = logger.With("job_id", jobID)
	if origin.IsValid() {
		logger = logger.With("origin_trace_id", origin.TraceID().String(), "origin_span_id", origin.SpanID().String())
	}
	logger.InfoContext(ctx, "shared result consumed")
	return consume(ctx)
}
