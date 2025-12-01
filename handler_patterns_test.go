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

package slogcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pjscruggs/slogcp"
)

// concurrencyTrackingWriter detects concurrent writes while capturing log output.
type concurrencyTrackingWriter struct {
	buf                 bytes.Buffer
	mu                  sync.Mutex
	activeWriters       int32
	concurrentWriteSeen int32
}

// Write records the supplied bytes while detecting overlapping writes.
func (w *concurrencyTrackingWriter) Write(p []byte) (int, error) {
	if atomic.AddInt32(&w.activeWriters, 1) > 1 {
		atomic.StoreInt32(&w.concurrentWriteSeen, 1)
	}
	defer atomic.AddInt32(&w.activeWriters, -1)

	// Yield briefly to increase the surface for overlapping writes when locks are missing.
	time.Sleep(500 * time.Microsecond)

	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

// String returns the buffered log output as a string.
func (w *concurrencyTrackingWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// ConcurrencyDetected reports whether overlapping writes were observed.
func (w *concurrencyTrackingWriter) ConcurrencyDetected() bool {
	return atomic.LoadInt32(&w.concurrentWriteSeen) != 0
}

// decodeLogEntries parses newline-delimited JSON records for assertions.
func decodeLogEntries(t *testing.T, raw string) []map[string]any {
	t.Helper()

	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}

	lines := strings.Split(raw, "\n")
	entries := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("json.Unmarshal(%q) returned %v", line, err)
		}
		entries = append(entries, entry)
	}
	return entries
}

// findEntryByMessage searches entries for one with the provided message.
func findEntryByMessage(entries []map[string]any, msg string) (map[string]any, bool) {
	for _, entry := range entries {
		if entry["message"] == msg {
			return entry, true
		}
	}
	return nil, false
}

// TestHandlerGlobalLoggingCompatibility validates slogcp can power slog's global
// logger and standard library redirects.
func TestHandlerGlobalLoggingCompatibility(t *testing.T) {
	var buf bytes.Buffer
	h, err := slogcp.NewHandler(io.Discard, slogcp.WithRedirectWriter(&buf))
	if err != nil {
		t.Fatalf("NewHandler() returned %v, want nil", err)
	}
	t.Cleanup(func() {
		if cerr := h.Close(); cerr != nil {
			t.Errorf("Handler.Close() returned %v, want nil", cerr)
		}
	})

	prevDefault := slog.Default()
	logger := slog.New(h)
	slog.SetDefault(logger)
	t.Cleanup(func() {
		slog.SetDefault(prevDefault)
	})

	slog.Info("global message", slog.String("pattern", "global"), slog.String("user", "alice"))

	stdLogger := slog.NewLogLogger(logger.Handler(), slog.LevelInfo)
	stdLogger.Print("std log line")

	entries := decodeLogEntries(t, buf.String())
	if len(entries) != 2 {
		t.Fatalf("expected 2 log entries, got %d (%v)", len(entries), entries)
	}

	globalEntry, ok := findEntryByMessage(entries, "global message")
	if !ok {
		t.Fatalf("global message entry missing: %v", entries)
	}
	if got := globalEntry["pattern"]; got != "global" {
		t.Fatalf("global entry pattern = %v, want global", got)
	}
	if got := globalEntry["user"]; got != "alice" {
		t.Fatalf("global entry user = %v, want alice", got)
	}

	stdEntry, ok := findEntryByMessage(entries, "std log line")
	if !ok {
		t.Fatalf("std log entry missing: %v", entries)
	}
	if _, exists := stdEntry["severity"]; !exists {
		t.Fatalf("std log entry missing severity: %v", stdEntry)
	}
}

// TestHandlerDependencyInjectedLoggingCompatibility confirms isolated loggers
// created via With* share synchronization and preserve copy-on-write semantics.
func TestHandlerDependencyInjectedLoggingCompatibility(t *testing.T) {
	writer := &concurrencyTrackingWriter{}
	h, err := slogcp.NewHandler(io.Discard, slogcp.WithRedirectWriter(writer))
	if err != nil {
		t.Fatalf("NewHandler() returned %v, want nil", err)
	}
	t.Cleanup(func() {
		if cerr := h.Close(); cerr != nil {
			t.Errorf("Handler.Close() returned %v, want nil", cerr)
		}
	})

	baseLogger := slog.New(h)
	baseLogger.Info("di base", slog.String("pattern", "di"))

	componentLogger := baseLogger.WithGroup("component").With(slog.String("name", "payments"))
	componentLogger.Info("component configured", slog.String("id", "root"))

	phaseLogger := componentLogger.With(slog.String("phase", "ingest"))

	const workers = 8
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := range workers {
		go func() {
			defer wg.Done()
			phaseLogger.Info("component run", slog.Int("iteration", i))
		}()
	}
	wg.Wait()

	if writer.ConcurrencyDetected() {
		t.Fatalf("detected overlapping writes from derived loggers, want serialized output")
	}

	entries := decodeLogEntries(t, writer.String())
	if len(entries) != workers+2 {
		t.Fatalf("expected %d entries, got %d (%v)", workers+2, len(entries), entries)
	}

	baseEntry, ok := findEntryByMessage(entries, "di base")
	if !ok {
		t.Fatalf("base entry missing: %v", entries)
	}
	if _, exists := baseEntry["component"]; exists {
		t.Fatalf("base logger unexpectedly mutated by WithGroup: %v", baseEntry)
	}

	componentEntry, ok := findEntryByMessage(entries, "component configured")
	if !ok {
		t.Fatalf("component entry missing: %v", entries)
	}
	componentMap, ok := componentEntry["component"].(map[string]any)
	if !ok {
		t.Fatalf("component entry lacks component grouping: %v", componentEntry)
	}
	if got := componentMap["name"]; got != "payments" {
		t.Fatalf("component name = %v, want payments", got)
	}
	if _, exists := componentMap["phase"]; exists {
		t.Fatalf("phase leaked to parent logger: %v", componentEntry)
	}

	seenIterations := make(map[int]struct{}, workers)
	for _, entry := range entries {
		if entry["message"] != "component run" {
			continue
		}
		comp, ok := entry["component"].(map[string]any)
		if !ok {
			t.Fatalf("component run entry missing component group: %v", entry)
		}
		if got := comp["name"]; got != "payments" {
			t.Fatalf("component run name = %v, want payments", got)
		}
		if got := comp["phase"]; got != "ingest" {
			t.Fatalf("component run phase = %v, want ingest", got)
		}
		iter, ok := comp["iteration"].(float64)
		if !ok {
			t.Fatalf("component run iteration missing or not numeric: %v", entry)
		}
		seenIterations[int(iter)] = struct{}{}
	}
	if len(seenIterations) != workers {
		t.Fatalf("expected %d unique iterations, saw %d (%v)", workers, len(seenIterations), seenIterations)
	}
}

// TestHandlerContextualLoggingCompatibility ensures contextual loggers remain
// isolated per request and safe for concurrent use.
func TestHandlerContextualLoggingCompatibility(t *testing.T) {
	writer := &concurrencyTrackingWriter{}
	h, err := slogcp.NewHandler(io.Discard, slogcp.WithRedirectWriter(writer))
	if err != nil {
		t.Fatalf("NewHandler() returned %v, want nil", err)
	}
	t.Cleanup(func() {
		if cerr := h.Close(); cerr != nil {
			t.Errorf("Handler.Close() returned %v, want nil", cerr)
		}
	})

	baseLogger := slog.New(h)

	type loggerKey struct{}
	loggerFromContext := func(ctx context.Context) *slog.Logger {
		if ctxLogger, ok := ctx.Value(loggerKey{}).(*slog.Logger); ok && ctxLogger != nil {
			return ctxLogger
		}
		return baseLogger
	}

	requests := []struct {
		id    string
		trace string
	}{
		{id: "alpha", trace: "trace-alpha"},
		{id: "beta", trace: "trace-beta"},
		{id: "gamma", trace: "trace-gamma"},
	}

	var wg sync.WaitGroup
	wg.Add(len(requests))
	for _, req := range requests {
		go func() {
			defer wg.Done()
			ctx := context.WithValue(context.Background(), loggerKey{}, baseLogger.With(
				slog.String("trace_id", req.trace),
				slog.String("request_id", req.id),
			))
			loggerFromContext(ctx).InfoContext(ctx, fmt.Sprintf("request %s handled", req.id), slog.String("path", "/"+req.id))
		}()
	}
	wg.Wait()

	loggerFromContext(context.Background()).Info("context fallback", slog.String("pattern", "base"))

	if writer.ConcurrencyDetected() {
		t.Fatalf("contextual logging triggered overlapping writes, want serialized output")
	}

	entries := decodeLogEntries(t, writer.String())
	if len(entries) != len(requests)+1 {
		t.Fatalf("expected %d entries, got %d (%v)", len(requests)+1, len(entries), entries)
	}

	for _, req := range requests {
		msg := fmt.Sprintf("request %s handled", req.id)
		entry, ok := findEntryByMessage(entries, msg)
		if !ok {
			t.Fatalf("missing request entry %q: %v", msg, entries)
		}
		if got := entry["trace_id"]; got != req.trace {
			t.Fatalf("trace_id for %q = %v, want %v", req.id, got, req.trace)
		}
		if got := entry["request_id"]; got != req.id {
			t.Fatalf("request_id for %q = %v, want %v", req.id, got, req.id)
		}
		if got := entry["path"]; got != "/"+req.id {
			t.Fatalf("path for %q = %v, want %s", req.id, got, "/"+req.id)
		}
	}

	fallbackEntry, ok := findEntryByMessage(entries, "context fallback")
	if !ok {
		t.Fatalf("fallback entry missing: %v", entries)
	}
	if _, exists := fallbackEntry["trace_id"]; exists {
		t.Fatalf("fallback entry unexpectedly carried contextual trace_id: %v", fallbackEntry)
	}
	if _, exists := fallbackEntry["request_id"]; exists {
		t.Fatalf("fallback entry unexpectedly carried contextual request_id: %v", fallbackEntry)
	}
	if got := fallbackEntry["pattern"]; got != "base" {
		t.Fatalf("fallback pattern = %v, want base", got)
	}
}
