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

package gcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/logging"
	"go.opentelemetry.io/otel/trace"

)

// mockEntryLogger captures logging.Entry without sending to external service.
type mockEntryLogger struct {
	mu      sync.Mutex
	entries []logging.Entry
}

func (m *mockEntryLogger) Log(e logging.Entry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if pm, ok := e.Payload.(map[string]any); ok {
		copyMap := make(map[string]any, len(pm))
		for k, v := range pm {
			copyMap[k] = v
		}
		e.Payload = copyMap
	}
	m.entries = append(m.entries, e)
}

func (m *mockEntryLogger) Flush() error { return nil }

func (m *mockEntryLogger) Reset() { m.mu.Lock(); m.entries = nil; m.mu.Unlock() }

func (m *mockEntryLogger) Last() (logging.Entry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.entries) == 0 {
		return logging.Entry{}, false
	}
	return m.entries[len(m.entries)-1], true
}


// contextWithTrace builds a context with remote span information.
func contextWithTrace(proj string) (context.Context, string, string, bool) {
	id := "11111111111111112222222222222222"
	sp := "3333333333333333"
	traceID, _ := trace.TraceIDFromHex(id)
	spanID, _ := trace.SpanIDFromHex(sp)
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled.WithSampled(true),
		Remote:     true,
	})
	ctx := trace.ContextWithRemoteSpanContext(context.Background(), sc)
	formatted := ""
	if proj != "" {
		formatted = "projects/" + proj + "/traces/" + id
	}
	return ctx, formatted, sp, proj != ""
}

func TestGcpHandler_Enabled(t *testing.T) {
	lv := new(slog.LevelVar)
	mockLog := &mockEntryLogger{}
	h := NewGcpHandler(Config{}, mockLog, lv)

	tests := []struct{ hl, rl slog.Level; want bool }{
		{slog.LevelInfo, slog.LevelDebug, false},
		{slog.LevelInfo, slog.LevelInfo, true},
		{slog.LevelDebug, slog.LevelDebug, true},
		{slog.LevelWarn, slog.LevelError, true},
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("H%v/R%v", tc.hl, tc.rl), func(t *testing.T) {
			lv.Set(tc.hl)
			got := h.Enabled(context.Background(), tc.rl)
			if got != tc.want {
				t.Errorf("Enabled(_, %v) = %v; want %v", tc.rl, got, tc.want)
			}
		})
	}

	hNil := NewGcpHandler(Config{}, mockLog, nil)
	if !hNil.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("nil leveler should enable Info")
	}
	if hNil.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("nil leveler should disable Debug")
	}
}

func TestGcpHandler_Handle_Basic(t *testing.T) {
	now := time.Now()
	r := slog.NewRecord(now, slog.LevelInfo, "hello", 0)
	log := &mockEntryLogger{}
	h := NewGcpHandler(Config{ProjectID: "proj"}, log, slog.LevelDebug)
	err := h.Handle(context.Background(), r)
	if err != nil {
		t.Fatalf("Handle error: %v", err)
	}
	entry, ok := log.Last()
	if !ok {
		t.Fatal("no entry recorded")
	}
	if entry.Severity != logging.Info {
		t.Errorf("Severity = %v; want Info", entry.Severity)
	}
	pm, _ := entry.Payload.(map[string]any)
	if pm[messageKey] != "hello" {
		t.Errorf("Payload[%q]=%v; want %q", messageKey, pm[messageKey], "hello")
	}
}

func TestGcpHandler_WithGroup(t *testing.T) {
	log := &mockEntryLogger{}
	h0 := NewGcpHandler(Config{ProjectID: "proj"}, log, nil)
	h1 := h0.WithGroup("g1").(*gcpHandler)
	// Ensure groups slice is copied, not shared
	if &h0.groups == &h1.groups {
		t.Error("groups slice was not copied")
	}
	r := slog.NewRecord(time.Now(), slog.LevelInfo, "x", 0)
	r.AddAttrs(slog.String("a", "b"))
	err := h1.Handle(context.Background(), r)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	entry, _ := log.Last()
	pm := entry.Payload.(map[string]any)
	gmap, ok := pm["g1"].(map[string]any)
	if !ok || gmap["a"] != "b" {
		t.Errorf("nested payload incorrect: %v", pm)
	}
}

func TestGcpHandler_WithAttrs(t *testing.T) {
	log := &mockEntryLogger{}
	h0 := NewGcpHandler(Config{ProjectID: "proj"}, log, nil)
	attrs := []slog.Attr{slog.String("k", "v"), slog.Int("n", 1)}
	h1 := h0.WithAttrs(attrs).(*gcpHandler)
	if &h0.groupedAttrs == &h1.groupedAttrs {
		t.Error("groupedAttrs slice was not copied")
	}
	r := slog.NewRecord(time.Now(), slog.LevelInfo, "m", 0)
	err := h1.Handle(context.Background(), r)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	entry, _ := log.Last()
	pm := entry.Payload.(map[string]any)
	if pm["k"] != "v" || pm["n"] != int64(1) {
		t.Errorf("attrs not applied: %v", pm)
	}
}

func TestGcpHandler_ReplaceAttrFunc(t *testing.T) {
	var seen [][]string
	replacer := func(gs []string, attr slog.Attr) slog.Attr {
		seen = append(seen, append([]string(nil), gs...))
		return attr
	}
	log := &mockEntryLogger{}
	h := NewGcpHandler(Config{ProjectID: "proj", ReplaceAttrFunc: replacer}, log, slog.LevelInfo)
	h = h.WithGroup("g1").WithGroup("g2").(*gcpHandler)
	r := slog.NewRecord(time.Now(), slog.LevelInfo, "z", 0)
	r.AddAttrs(slog.String("x", "y"))
	err := h.Handle(context.Background(), r)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(seen) == 0 || len(seen[0]) != 2 {
		t.Errorf("expected replacer called with 2 groups, got %v", seen)
	}
}

func TestGcpHandler_TraceAndError(t *testing.T) {
	project := "proj"
	ctx, wantTrace, wantSpan, wantSample := contextWithTrace(project)
	log := &mockEntryLogger{}
	h := NewGcpHandler(Config{ProjectID: project}, log, slog.LevelDebug)
	r := slog.NewRecord(time.Now(), slog.LevelInfo, "t", 0)
	err := h.Handle(ctx, r)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	entry, _ := log.Last()
	if entry.Trace != wantTrace {
		t.Errorf("Trace = %q; want %q", entry.Trace, wantTrace)
	}
	if entry.SpanID != wantSpan {
		t.Errorf("SpanID = %q; want %q", entry.SpanID, wantSpan)
	}
	if entry.TraceSampled != wantSample {
		t.Errorf("Sampled = %v; want %v", entry.TraceSampled, wantSample)
	}
}

func TestGcpHandler_HTTPRequestAttr(t *testing.T) {
	req, err := http.NewRequest("GET", "/", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	hreq := &logging.HTTPRequest{Request: req, Status: 200}
	log := &mockEntryLogger{}
	h := NewGcpHandler(Config{ProjectID: "p"}, log, slog.LevelDebug)
	r := slog.NewRecord(time.Now(), slog.LevelInfo, "h", 0)
	r.AddAttrs(slog.Any(httpRequestKey, hreq))
	err = h.Handle(context.Background(), r)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	entry, _ := log.Last()
	if entry.HTTPRequest == nil {
		t.Error("expected HTTPRequest in entry")
	}
	pm := entry.Payload.(map[string]any)
	if _, ok := pm[httpRequestKey]; ok {
		t.Error("HTTPRequest should be removed from payload map")
	}
}

func TestGcpHandler_ErrorLogging(t *testing.T) {
	errVal := errors.New("fail")
	log := &mockEntryLogger{}
	h := NewGcpHandler(Config{ProjectID: "p", StackTraceEnabled: true, StackTraceLevel: slog.LevelError}, log, slog.LevelDebug)
	r := slog.NewRecord(time.Now(), slog.LevelError, "e", getTestPC(t))
	r.AddAttrs(slog.Any("err", errVal))
	err := h.Handle(context.Background(), r)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	entry, _ := log.Last()
	pm := entry.Payload.(map[string]any)
	if pm[errorTypeKey] == nil {
		t.Error("expected errorTypeKey in payload")
	}
	if _, ok := pm[stackTraceKey].(string); !ok {
		t.Error("expected stackTraceKey in payload")
	}
}
