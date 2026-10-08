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

// Package correlation demonstrates HTTP, detached-job, and shared-work tracing.
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

// job contains owned application data and tracing identity, never a request
// context or its logger. A real queue also needs its own delivery guarantees.
type job struct {
	id      string
	origin  trace.SpanContext
	project string
}

// captureJob snapshots only approved identity from the submitting operation.
func captureJob(ctx context.Context, id string) job {
	project, _ := slogcp.TraceProjectIDFromContext(ctx)
	return job{id: id, origin: trace.SpanContextFromContext(ctx), project: project}
}

// runJob continues the origin trace under worker cancellation and a job timeout.
// base is explicitly selected by the application and must be non-nil.
func runJob(workerCtx context.Context, base *slog.Logger, tracer trace.Tracer, queued job, timeout time.Duration, process func(context.Context) error) error {
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
