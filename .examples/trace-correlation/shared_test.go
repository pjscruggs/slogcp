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
	"bytes"
	"context"
	"log/slog"
	"testing"

	"github.com/pjscruggs/slogcp/v2"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// TestSharedWork checks links independently from parentage and log correlation.
func TestSharedWork(t *testing.T) {
	t.Parallel()
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	tracer := provider.Tracer("shared-work")
	_, originSpan := tracer.Start(context.Background(), "origin request")
	origin := originSpan.SpanContext()
	originSpan.End()
	jobCtx, jobSpan := tracer.Start(context.Background(), "shared job", trace.WithNewRoot(), trace.WithLinks(trace.Link{SpanContext: origin}))
	work := jobSpan.SpanContext()
	var logs bytes.Buffer
	handler, err := slogcp.NewHandler(&logs, slogcp.WithRedirectWriter(&logs), slogcp.WithTraceProjectID("test-project"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = handler.Close() })
	logger := slog.New(handler).With("job_id", "shared-42", "origin_trace_id", origin.TraceID().String(), "origin_span_id", origin.SpanID().String())
	logger.InfoContext(jobCtx, "shared job ready")
	jobSpan.End()
	consumerCtx, consumerSpan := tracer.Start(context.Background(), "consumer request")
	defer consumerSpan.End()
	var consumption trace.SpanContext
	err = consumeShared(consumerCtx, slog.New(handler), tracer, "shared-42", origin, work, func(ctx context.Context) error {
		consumption = trace.SpanContextFromContext(ctx)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	entries := readLogs(t, &logs)
	if len(entries) != 2 {
		t.Fatalf("got %d logs, want 2", len(entries))
	}
	for i, sc := range []trace.SpanContext{work, consumption} {
		entry := entries[i]
		if entry[slogcp.TraceKey] != "projects/test-project/traces/"+sc.TraceID().String() || entry[slogcp.SpanKey] != sc.SpanID().String() || entry["job_id"] != "shared-42" || entry["origin_trace_id"] != origin.TraceID().String() || entry["origin_span_id"] != origin.SpanID().String() {
			t.Fatalf("incorrect shared-work log fields: %v", entry)
		}
	}
	if consumption.TraceID() != consumerSpan.SpanContext().TraceID() || work.TraceID() == origin.TraceID() || work.TraceID() == consumption.TraceID() {
		t.Fatal("independent shared work and consumer traces were conflated")
	}
	spans := exporter.GetSpans()
	if len(spans) != 3 {
		t.Fatalf("got %d ended spans, want origin, job, and consumption", len(spans))
	}
	for _, span := range spans {
		switch span.Name {
		case "shared job":
			if span.Parent.IsValid() || len(span.Links) != 1 || !span.Links[0].SpanContext.Equal(origin) {
				t.Fatal("shared job must be a new root linked to its origin")
			}
		case "consume shared job":
			if !span.Parent.Equal(consumerSpan.SpanContext()) || len(span.Links) != 1 || !span.Links[0].SpanContext.Equal(work) {
				t.Fatal("consumption must be a consumer child linked to shared work")
			}
		}
	}
	exporter.Reset()
	logs.Reset()
	if err := consumeShared(context.Background(), slog.New(handler), tracer, "unlinked", trace.SpanContext{}, trace.SpanContext{}, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	spans = exporter.GetSpans()
	if len(spans) != 1 || spans[0].Parent.IsValid() || len(spans[0].Links) != 0 {
		t.Fatal("invalid work identity produced a parent or link")
	}
	if _, exists := readLogs(t, &logs)[0]["origin_trace_id"]; exists {
		t.Fatal("invalid origin produced origin log fields")
	}
}
