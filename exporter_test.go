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

package slogcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"
)

type testEntryExporter func(context.Context, Entry) error

// Export invokes the test's entry consumer synchronously.
func (f testEntryExporter) Export(ctx context.Context, entry Entry) error {
	return f(ctx, entry)
}

// TestExporterMatchesJSON verifies the shared enrichment boundary, including
// metadata promotion, replacement, groups, labels, errors, and trace context.
func TestExporterMatchesJSON(t *testing.T) {
	clearHandlerEnv(t)
	traceID, _ := trace.TraceIDFromHex("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	spanID, _ := trace.SpanIDFromHex("bbbbbbbbbbbbbbbb")
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled,
	}))
	var exported []byte
	exporter := testEntryExporter(func(gotCtx context.Context, entry Entry) error {
		if gotCtx != ctx || entry.Level != slog.LevelInfo {
			t.Error("exporter lost context or level")
		}
		for _, key := range []string{"severity", "time", TraceKey, SpanKey, SampledKey, LabelsGroup, httpRequestKey} {
			if _, exists := entry.Payload[key]; exists {
				t.Errorf("metadata %q remains in payload", key)
			}
		}
		payload := make(map[string]any, len(entry.Payload)+8)
		maps.Copy(payload, entry.Payload)
		payload["severity"] = entry.Severity
		payload["time"] = entry.Timestamp.UTC().Format(time.RFC3339Nano)
		payload[TraceKey], payload[SpanKey], payload[SampledKey] = entry.Trace, entry.SpanID, entry.TraceSampled
		payload[LabelsGroup], payload[httpRequestKey] = entry.Labels, entry.HTTPRequest
		var err error
		exported, err = json.Marshal(payload)
		return err
	})
	opts := []Option{WithTraceProjectID("example-project"), WithTime(true), WithSeverityAliases(false),
		WithSourceLocationEnabled(false), WithStackTraceEnabled(false), WithAttrs([]slog.Attr{slog.String("base", "value")}),
		WithReplaceAttr(func(_ []string, attr slog.Attr) slog.Attr {
			if attr.Key == "secret" {
				return slog.String("secret", "redacted")
			}
			return attr
		}),
	}
	var output bytes.Buffer
	jsonHandler, err := NewHandler(&output, opts...)
	if err != nil {
		t.Fatal(err)
	}
	exportHandler, err := NewHandlerWithExporter(exporter, opts...)
	if err != nil {
		t.Fatal(err)
	}
	record := slog.NewRecord(time.Unix(100, 123), slog.LevelInfo, "request", 0)
	record.AddAttrs(slog.Group("nested", slog.String("secret", "hidden")),
		slog.Group(LabelsGroup, slog.String("region", "example-region")),
		slog.Any("httpRequest", &HTTPRequest{RequestMethod: "GET", RequestURL: "https://example.com", Status: 200}),
		slog.Any("error", errors.New("failure")))
	if err := jsonHandler.Handle(ctx, record); err != nil {
		t.Fatal(err)
	}
	if err := exportHandler.Handle(ctx, record); err != nil {
		t.Fatal(err)
	}
	var want, got map[string]any
	if err := json.Unmarshal(output.Bytes(), &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(exported, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("exported = %#v; JSON = %#v", got, want)
	}
}

// TestExporterPromotesHTTPRequest checks normalized HTTP metadata with and without
// attribute replacement, including the lazy value used by HTTP integrations.
func TestExporterPromotesHTTPRequest(t *testing.T) {
	clearHandlerEnv(t)
	for _, replace := range []bool{false, true} {
		for _, form := range []string{"request", "lazy", "map"} {
			name := form
			if replace {
				name += " with replacement"
			}
			t.Run(name, func(t *testing.T) {
				request := httptest.NewRequestWithContext(t.Context(), "POST", "https://example.com/jobs?q=1", nil)
				request.Header.Set("User-Agent", "exporter-test")
				request.RemoteAddr = "192.0.2.1:1234"
				metadata := &HTTPRequest{Request: request, Status: 201, ResponseSize: 42, Latency: 5 * time.Millisecond}
				attr := slog.Any(httpRequestKey, metadata)
				switch form {
				case "lazy":
					attr.Value = HTTPRequestValue(func() *HTTPRequest { return metadata })
				case "map":
					attr.Value = metadata.LogValue()
				}
				var got map[string]any
				opts := []Option{WithSourceLocationEnabled(false)}
				if replace {
					opts = append(opts, WithReplaceAttr(func(_ []string, attr slog.Attr) slog.Attr { return attr }))
				}
				handler, err := NewHandlerWithExporter(testEntryExporter(func(_ context.Context, entry Entry) error {
					if _, exists := entry.Payload[httpRequestKey]; exists {
						t.Errorf("HTTP metadata remains in the application payload")
					}
					got = maps.Clone(entry.HTTPRequest)
					return nil
				}), opts...)
				if err != nil {
					t.Fatal(err)
				}
				record := slog.NewRecord(time.Now(), slog.LevelInfo, "request", 0)
				record.AddAttrs(attr)
				if err := handler.Handle(context.Background(), record); err != nil {
					t.Fatal(err)
				}
				if err := handler.Close(); err != nil {
					t.Fatal(err)
				}
				if got["requestMethod"] != "POST" || got["requestUrl"] != "https://example.com/jobs?q=1" ||
					got["status"] != 201 || got["responseSize"] != "42" || got["latency"] != "0.005000000s" ||
					got["remoteIp"] != "192.0.2.1" || got["userAgent"] != "exporter-test" {
					t.Fatalf("normalized HTTP metadata = %#v", got)
				}
			})
		}
	}
}

// TestExporterConstructionAndOwnership prevents file redirects or automatic
// file buffering from accidentally taking over an exporter handler.
func TestExporterConstructionAndOwnership(t *testing.T) {
	clearHandlerEnv(t)
	if _, err := NewHandlerWithExporter(nil); err == nil {
		t.Fatal("nil exporter accepted")
	}
	path := filepath.Join(t.TempDir(), "must-not-exist.log")
	t.Setenv(envTarget, "file:"+path)
	var called bool
	wantErr := errors.New("export rejected")
	h, err := NewHandlerWithExporter(testEntryExporter(func(_ context.Context, entry Entry) error {
		called = true
		if !entry.Timestamp.IsZero() || entry.Severity != "WARNING" {
			t.Errorf("unexpected metadata: %+v", entry)
		}
		if _, ok := entry.Payload["unsupported"].(chan int); !ok {
			t.Error("payload was encoded before export")
		}
		return wantErr
	}), WithRedirectToFile(path), WithAsyncOnFile(), WithTime(false), WithSeverityAliases(true))
	if err != nil {
		t.Fatal(err)
	}
	if h.asyncHandler != nil {
		t.Fatal("file async queue enabled for exporter")
	}
	r := slog.NewRecord(time.Now(), slog.LevelWarn, "test", 0)
	r.AddAttrs(slog.Any("unsupported", make(chan int)))
	if err := h.Handle(context.Background(), r); !errors.Is(err, wantErr) {
		t.Fatalf("Handle error = %v", err)
	}
	if !called {
		t.Fatal("exporter not called")
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("redirect file exists or stat failed: %v", err)
	}
}

// TestExporterClonesAndAsyncDrain checks concurrent clones, level filtering,
// and draining the slogcp queue before the caller flushes its exporter.
func TestExporterClonesAndAsyncDrain(t *testing.T) {
	clearHandlerEnv(t)
	var calls atomic.Int64
	var bad atomic.Bool
	h, err := NewHandlerWithExporter(testEntryExporter(func(_ context.Context, entry Entry) error {
		group, ok := entry.Payload["group"].(map[string]any)
		if !ok || group["base"] != "value" || group["n"] == nil {
			bad.Store(true)
		}
		calls.Add(1)
		return nil
	}), WithLevel(slog.LevelInfo), WithSourceLocationEnabled(false), WithAsync())
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(h).WithGroup("group").With("base", "value")
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			for n := range 40 {
				logger.Info("accepted", "n", n)
				logger.Debug("filtered", "n", n)
			}
		})
	}
	workers.Wait()
	if err := h.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 320 || bad.Load() {
		t.Fatalf("calls=%d bad=%v", calls.Load(), bad.Load())
	}
}

// TestExporterMetadataFallback keeps unexpected reserved attributes available
// to transports instead of silently losing application data.
func TestExporterMetadataFallback(t *testing.T) {
	t.Parallel()
	source := &SourceLocation{File: "example.go", Line: 42, Function: "example"}
	h := &jsonHandler{cfg: &handlerConfig{}}
	payload := map[string]any{"logging.googleapis.com/sourceLocation": source, TraceKey: 42, "time": "application-time", httpRequestKey: (*httpRequestPayload)(nil)}
	entry := h.exportEntry(slog.NewRecord(time.Time{}, slog.LevelInfo+1, "", 0), payload)
	if entry.SourceLocation != source || entry.Payload[TraceKey] != 42 || entry.Payload["time"] != "application-time" || entry.Level != slog.LevelInfo+1 {
		t.Fatalf("unexpected entry: %+v", entry)
	}
	if _, exists := entry.Payload[httpRequestKey]; exists || entry.HTTPRequest != nil {
		t.Fatal("nil HTTP metadata was not removed")
	}
}
