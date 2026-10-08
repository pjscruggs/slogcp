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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pjscruggs/slogcp/v2"
	"github.com/pjscruggs/slogcp/v2/slogcphttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// requestValueKey identifies a request-only value that detached jobs must discard.
type requestValueKey struct{}

// TestCorrelation exercises each logger selection style, with a deterministic
// parent-based sampler. Tests changing process defaults must remain serial.
func TestCorrelation(t *testing.T) {
	for _, style := range []string{"global", "injected", "context"} {
		for _, sampled := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/sampled=%t", style, sampled), func(t *testing.T) {
				testCorrelation(t, style, sampled)
			})
		}
	}
}

// testCorrelation checks the application-visible identities as well as SDK output.
func testCorrelation(t *testing.T, style string, sampled bool) {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter), sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())))
	previousProvider, previousLogger := otel.GetTracerProvider(), slog.Default()
	t.Cleanup(func() {
		otel.SetTracerProvider(previousProvider)
		slog.SetDefault(previousLogger)
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	otel.SetTracerProvider(provider)
	tracer := provider.Tracer("correlation-recipe")
	var logs bytes.Buffer
	handler, err := slogcp.NewHandler(&logs, slogcp.WithRedirectWriter(&logs), slogcp.WithTraceProjectID("test-project"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := handler.Close(); err != nil {
			t.Error(err)
		}
	})
	base := slog.New(handler).With("component", "recipe", "pattern", style)
	slog.SetDefault(base)
	identities := make(map[string]trace.SpanContext)
	logEvent := func(ctx context.Context, message, jobID string) {
		identities[message] = trace.SpanContextFromContext(ctx)
		switch style {
		case "global":
			slog.InfoContext(ctx, message, "job_id", jobID)
		case "injected":
			base.InfoContext(ctx, message, "job_id", jobID)
		case "context":
			slogcp.Logger(ctx).InfoContext(ctx, message, "job_id", jobID)
		}
	}
	var queued job
	requestHandler := slogcphttp.Middleware(
		slogcphttp.WithLogger(base.With("request_only", true)),
		slogcphttp.WithProjectID("test-project"),
		slogcphttp.WithPropagators(propagation.TraceContext{}),
	)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		ctx := slogcp.ContextWithTraceProjectID(r.Context(), "test-project")
		logEvent(ctx, "request", "job-42")
		queued = captureJob(ctx, "job-42")
		childCtx, child := tracer.Start(ctx, "request child")
		logEvent(childCtx, "request child", "job-42")
		child.End()
	}))
	member, err := baggage.NewMember("request-secret", "do-not-copy")
	if err != nil {
		t.Fatal(err)
	}
	bag, err := baggage.New(member)
	if err != nil {
		t.Fatal(err)
	}
	requestCtx, cancelRequest := context.WithCancel(context.WithValue(baggage.ContextWithBaggage(context.Background(), bag), requestValueKey{}, "pinned connection"))
	defer cancelRequest()
	request := httptest.NewRequestWithContext(requestCtx, http.MethodGet, "https://example.com/work", nil)
	flag := "0"
	if sampled {
		flag = "1"
	}
	request.Header.Set(slogcphttp.XCloudTraceContextHeader, "105445aa7843bc8bf206b12000100000/10;o="+flag)
	requestHandler.ServeHTTP(httptest.NewRecorder(), request)
	cancelRequest() // The server span has ended, and the originating request is canceled.
	err = runJob(context.Background(), base, tracer, queued, time.Second, func(ctx context.Context) error {
		if ctx.Err() != nil || ctx.Value(requestValueKey{}) != nil || baggage.FromContext(ctx).Len() != 0 {
			t.Fatal("detached work retained request cancellation, values, or baggage")
		}
		if project, ok := slogcp.TraceProjectIDFromContext(ctx); !ok || project != queued.project {
			t.Fatal("detached work lost its trace-project override")
		}
		logEvent(ctx, "job", queued.id)
		childCtx, child := tracer.Start(ctx, "job child")
		defer child.End()
		logEvent(childCtx, "job child", queued.id)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[trace.SpanID]bool)
	for _, name := range []string{"request", "request child", "job", "job child"} {
		id := identities[name].SpanID()
		if seen[id] {
			t.Fatalf("%s reused another operation's span ID", name)
		}
		seen[id] = true
	}
	identities["job started"], identities["job completed"] = identities["job"], identities["job"]
	entries := readLogs(t, &logs)
	if len(entries) != 6 {
		t.Fatalf("got %d logs, want 6", len(entries))
	}
	for _, entry := range entries {
		message, _ := entry["message"].(string)
		sc := identities[message]
		if !sc.IsValid() || sc.IsRemote() || sc.TraceID() != queued.origin.TraceID() {
			t.Fatalf("incorrect active context for %s: %v", message, sc)
		}
		if entry[slogcp.TraceKey] != "projects/test-project/traces/"+sc.TraceID().String() || entry[slogcp.SpanKey] != sc.SpanID().String() || entry[slogcp.SampledKey] != sampled {
			t.Fatalf("incorrect serialized correlation for %s: %v", message, entry)
		}
		if entry["component"] != "recipe" || entry["pattern"] != style || entry["job_id"] != queued.id {
			t.Fatalf("lost selected logger fields: %v", entry)
		}
		requestEvent := message == "request" || message == "request child"
		_, hasRequestFields := entry["request_only"]
		_, hasHTTPFields := entry["http.target"]
		if hasRequestFields != (style == "context" && requestEvent) || hasHTTPFields != hasRequestFields {
			t.Fatalf("logger selection inherited incorrect request fields: %v", entry)
		}
	}
	spans := exporter.GetSpans()
	if !sampled {
		if len(spans) != 0 {
			t.Fatal("unsampled trace exported spans")
		}
		return // Correlated JSON above exists even though the exporter is empty.
	}
	if len(spans) != 4 {
		t.Fatalf("got %d spans, want 4", len(spans))
	}
	for _, span := range spans {
		var parent trace.SpanContext
		switch span.Name {
		case "request child", "process detached job":
			parent = identities["request"]
		case "job child":
			parent = identities["job"]
		default:
			originSpan, parseErr := trace.SpanIDFromHex("000000000000000a")
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			parent = trace.NewSpanContext(trace.SpanContextConfig{TraceID: queued.origin.TraceID(), SpanID: originSpan, Remote: true, TraceFlags: trace.FlagsSampled})
		}
		if !span.Parent.Equal(parent) || len(span.Links) != 0 {
			t.Fatalf("incorrect parent or links for %s: %v", span.Name, span)
		}
	}
}

// readLogs decodes the JSON stream instead of testing strings or log ordering.
func readLogs(t *testing.T, reader io.Reader) []map[string]any {
	t.Helper()
	decoder := json.NewDecoder(reader)
	var entries []map[string]any
	for {
		var entry map[string]any
		if err := decoder.Decode(&entry); errors.Is(err, io.EOF) {
			return entries
		} else if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, entry)
	}
}

// TestJobLifetime verifies cancellation belongs to the worker, not the request.
func TestJobLifetime(t *testing.T) {
	t.Parallel()
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	tracer := provider.Tracer("job-lifetime")
	workerCtx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	process := func(context.Context) error { called = true; return nil }
	if err := runJob(workerCtx, logger, tracer, job{}, time.Second, process); !errors.Is(err, context.Canceled) || called {
		t.Fatal("canceled worker processed a job")
	}
	if err := runJob(context.Background(), logger, tracer, job{}, 0, process); !errors.Is(err, context.DeadlineExceeded) || called {
		t.Fatal("expired deadline processed a job")
	}
	workerCtx, cancel = context.WithCancel(context.Background())
	defer cancel()
	err := runJob(workerCtx, logger, tracer, job{}, time.Second, func(ctx context.Context) error {
		cancel()
		<-ctx.Done()
		return ctx.Err()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("worker cancellation did not reach processing: %v", err)
	}
	spans := exporter.GetSpans()
	if len(spans) != 1 || spans[0].Parent.IsValid() || spans[0].Status.Code != codes.Error {
		t.Fatal("invalid origin or failure span has incorrect parent/status")
	}
	entries := readLogs(t, &logs)
	if len(entries) != 2 || entries[1]["msg"] != "job failed" || entries[1]["level"] != "ERROR" {
		t.Fatal("processing failure did not emit an error log")
	}
	if err := runJob(context.Background(), logger, tracer, job{}, time.Millisecond, func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("job deadline did not reach processing: %v", err)
	}
	if err := runJob(context.Background(), logger, tracer, job{}, time.Second, func(ctx context.Context) error {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("job lacks a processing deadline")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
