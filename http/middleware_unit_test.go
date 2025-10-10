// Copyright 2025 Patrick J. Scruggs
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

package http

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

func TestMiddlewareSuppressUnsampledBelowRespectsServerErrors(t *testing.T) {
	previous := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTextMapPropagator(previous)
	})

	logger, records := newRecordingLogger()
	mw := Middleware(logger, WithSuppressUnsampledBelow(slog.Level(30)))

	req := httptest.NewRequest(http.MethodGet, "http://example.com/ok", nil)
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00")

	rr := httptest.NewRecorder()
	mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rr, req)

	if count := records.Count(); count != 0 {
		t.Fatalf("unsampled 200 request logged %d records, want 0", count)
	}

	req = httptest.NewRequest(http.MethodGet, "http://example.com/error", nil)
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00")
	rr = httptest.NewRecorder()

	mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})).ServeHTTP(rr, req)

	if count := records.Count(); count != 1 {
		t.Fatalf("unsampled 500 request logged %d records, want 1", count)
	}
}

type recordingState struct {
	mu      sync.Mutex
	records []slog.Record
}

func newRecordingLogger() (*slog.Logger, *recordingState) {
	state := &recordingState{}
	logger := slog.New(&recordingHandler{state: state})
	return logger, state
}

type recordingHandler struct {
	state *recordingState
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool {
	return true
}

func (h *recordingHandler) Handle(_ context.Context, record slog.Record) error {
	clone := slog.NewRecord(record.Time, record.Level, record.Message, record.PC)
	record.Attrs(func(attr slog.Attr) bool {
		clone.AddAttrs(attr)
		return true
	})
	h.state.mu.Lock()
	h.state.records = append(h.state.records, clone)
	h.state.mu.Unlock()
	return nil
}

func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler {
	return &recordingHandler{state: h.state}
}

func (h *recordingHandler) WithGroup(string) slog.Handler {
	return &recordingHandler{state: h.state}
}

func (s *recordingState) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.records)
}
