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

package slogcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pjscruggs/slogcp/slogcpasync"
)

// closingBuffer tracks whether Close is invoked on an io.Writer stand-in.
type closingBuffer struct {
	bytes.Buffer
	closed bool
}

// Close marks the buffer closed for assertions in tests.
func (c *closingBuffer) Close() error {
	c.closed = true
	return nil
}

// prefersManagedDefaults reports whether runtime detection mirrors managed GCP
// platforms where slogcp opts into Cloud Logging defaults.
func prefersManagedDefaults() bool {
	switch DetectRuntimeInfo().Environment {
	case RuntimeEnvCloudRunService,
		RuntimeEnvCloudRunJob,
		RuntimeEnvCloudFunctions,
		RuntimeEnvAppEngineStandard,
		RuntimeEnvAppEngineFlexible:
		return true
	default:
		return false
	}
}

// decodeLogBuffer splits JSON log lines and converts them into maps for easier assertions.
func decodeLogBuffer(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	content := strings.TrimSpace(buf.String())
	if content == "" {
		return nil
	}

	lines := strings.Split(content, "\n")
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

// TestNewHandlerWithRedirectWriter verifies that redirect writers receive log entries.
func TestNewHandlerWithRedirectWriter(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	h, err := NewHandler(io.Discard, WithRedirectWriter(&buf))
	if err != nil {
		t.Fatalf("NewHandler() returned %v, want nil", err)
	}
	t.Cleanup(func() {
		if cerr := h.Close(); cerr != nil {
			t.Errorf("Handler.Close() returned %v, want nil", cerr)
		}
	})

	logger := slog.New(h)
	logger.InfoContext(context.Background(), "hello", slog.String("key", "value"))

	line := buf.String()
	if !strings.Contains(line, `"message":"hello"`) {
		t.Fatalf("log output %q missing message", line)
	}
	if !strings.Contains(line, `"key":"value"`) {
		t.Fatalf("log output %q missing attribute", line)
	}
}

// TestHandlerCloseDoesNotCloseRedirectWriter ensures Close leaves caller-supplied writers open.
func TestHandlerCloseDoesNotCloseRedirectWriter(t *testing.T) {
	t.Parallel()

	cw := &closingBuffer{}
	h, err := NewHandler(io.Discard, WithRedirectWriter(cw))
	if err != nil {
		t.Fatalf("NewHandler() returned %v, want nil", err)
	}

	if err := h.Close(); err != nil {
		t.Fatalf("Handler.Close() returned %v, want nil", err)
	}
	if cw.closed {
		t.Fatalf("redirect writer was unexpectedly closed")
	}
}

// TestReplaceAttrRunsOnRecordAttributes ensures replacers see record-scoped attributes and nested groups.
func TestReplaceAttrRunsOnRecordAttributes(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	var seenDynamic, seenNested bool
	h, err := NewHandler(io.Discard,
		WithRedirectWriter(&buf),
		WithReplaceAttr(func(groups []string, attr slog.Attr) slog.Attr {
			switch attr.Key {
			case "dynamic":
				if len(groups) != 0 {
					t.Fatalf("dynamic attr groups = %v, want none", groups)
				}
				seenDynamic = true
				return slog.String(attr.Key, "rewritten")
			case "child":
				if len(groups) != 1 || groups[0] != "group" {
					t.Fatalf("child attr groups = %v, want [group]", groups)
				}
				seenNested = true
				return slog.String(attr.Key, "nested-rewritten")
			default:
				return attr
			}
		}),
	)
	if err != nil {
		t.Fatalf("NewHandler() returned %v, want nil", err)
	}
	t.Cleanup(func() {
		if cerr := h.Close(); cerr != nil {
			t.Errorf("Handler.Close() returned %v, want nil", cerr)
		}
	})

	logger := slog.New(h)
	logger.InfoContext(context.Background(), "replace", slog.String("dynamic", "value"),
		slog.Group("group", slog.String("child", "value")))

	if !seenDynamic || !seenNested {
		t.Fatalf("ReplaceAttr did not observe expected attrs: dynamic=%v nested=%v", seenDynamic, seenNested)
	}

	entries := decodeLogBuffer(t, &buf)
	if len(entries) != 1 {
		t.Fatalf("expected 1 log entry, got %d (%v)", len(entries), entries)
	}

	if got := entries[0]["dynamic"]; got != "rewritten" {
		t.Fatalf("dynamic attr = %v, want rewritten", got)
	}

	groupAny, ok := entries[0]["group"]
	if !ok {
		t.Fatalf("group not present in entry: %v", entries[0])
	}
	groupMap, ok := groupAny.(map[string]any)
	if !ok {
		t.Fatalf("group attr type = %T, want map[string]any", groupAny)
	}
	if got := groupMap["child"]; got != "nested-rewritten" {
		t.Fatalf("group child attr = %v, want nested-rewritten", got)
	}
}

// TestHandlerReopenLogFile confirms log files rotate without losing entries.
func TestHandlerReopenLogFile(t *testing.T) {
	t.Parallel()

	logDir := t.TempDir()
	logPath := filepath.Join(logDir, "app.log")

	h, err := NewHandler(io.Discard, WithRedirectToFile(logPath))
	if err != nil {
		t.Fatalf("NewHandler() returned %v, want nil", err)
	}
	t.Cleanup(func() {
		if cerr := h.Close(); cerr != nil {
			t.Errorf("Handler.Close() returned %v, want nil", cerr)
		}
	})

	logger := slog.New(h)
	logger.InfoContext(context.Background(), "first")

	if err := h.ReopenLogFile(); err != nil {
		t.Fatalf("ReopenLogFile() returned %v, want nil", err)
	}

	logger.InfoContext(context.Background(), "second")

	if err := h.Close(); err != nil {
		t.Fatalf("Close() returned %v, want nil", err)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile(%q) returned %v", logPath, err)
	}
	content := string(data)
	if !strings.Contains(content, `"message":"first"`) || !strings.Contains(content, `"message":"second"`) {
		t.Fatalf("log file content missing expected entries: %s", content)
	}
}

// TestLoggerWithAttrsDoesNotLeakToParent checks With(attrs) scope is limited to child loggers.
func TestLoggerWithAttrsDoesNotLeakToParent(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	h, err := NewHandler(io.Discard, WithRedirectWriter(&buf))
	if err != nil {
		t.Fatalf("NewHandler() returned %v, want nil", err)
	}
	t.Cleanup(func() {
		if cerr := h.Close(); cerr != nil {
			t.Errorf("Handler.Close() returned %v, want nil", cerr)
		}
	})

	base := slog.New(h)
	ctx := context.Background()

	derived := base.With(slog.String("request_id", "abc123"))

	base.InfoContext(ctx, "base")
	derived.InfoContext(ctx, "child")
	base.InfoContext(ctx, "after")

	entries := decodeLogBuffer(t, &buf)
	if len(entries) != 3 {
		t.Fatalf("expected 3 log entries, got %d (%v)", len(entries), entries)
	}

	if _, ok := entries[0]["request_id"]; ok {
		t.Fatalf("parent logger unexpectedly contains request_id in first entry: %v", entries[0])
	}

	if got := entries[1]["request_id"]; got != "abc123" {
		t.Fatalf("child logger missing request_id, got %v", got)
	}

	if _, ok := entries[2]["request_id"]; ok {
		t.Fatalf("parent logger unexpectedly contains request_id in final entry: %v", entries[2])
	}
}

// TestLoggerWithGroupDoesNotLeakToParent checks WithGroup isolates nested attributes.
func TestLoggerWithGroupDoesNotLeakToParent(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	h, err := NewHandler(io.Discard, WithRedirectWriter(&buf))
	if err != nil {
		t.Fatalf("NewHandler() returned %v, want nil", err)
	}
	t.Cleanup(func() {
		if cerr := h.Close(); cerr != nil {
			t.Errorf("Handler.Close() returned %v, want nil", cerr)
		}
	})

	base := slog.New(h)
	ctx := context.Background()

	grouped := base.WithGroup("request")

	base.InfoContext(ctx, "base", slog.String("id", "root"))
	grouped.InfoContext(ctx, "child", slog.String("id", "grouped"))
	base.InfoContext(ctx, "after", slog.String("id", "final"))

	entries := decodeLogBuffer(t, &buf)
	if len(entries) != 3 {
		t.Fatalf("expected 3 log entries, got %d (%v)", len(entries), entries)
	}

	if _, ok := entries[0]["request"].(map[string]any); ok {
		t.Fatalf("parent logger unexpectedly contains request group in first entry: %v", entries[0])
	}

	groupedField, ok := entries[1]["request"].(map[string]any)
	if !ok {
		t.Fatalf("child logger missing request group: %v", entries[1])
	}
	if got := groupedField["id"]; got != "grouped" {
		t.Fatalf("child logger request.id = %v, want grouped", got)
	}

	if _, ok := entries[2]["request"].(map[string]any); ok {
		t.Fatalf("parent logger unexpectedly contains request group in final entry: %v", entries[2])
	}
	if got := entries[2]["id"]; got != "final" {
		t.Fatalf("parent log id = %v, want final", got)
	}
}

// TestHandlerSetLevel verifies that SetLevel adjusts runtime filtering.
func TestHandlerSetLevel(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	h, err := NewHandler(io.Discard, WithRedirectWriter(&buf), WithLevel(slog.LevelInfo))
	if err != nil {
		t.Fatalf("NewHandler() returned %v, want nil", err)
	}
	t.Cleanup(func() {
		if cerr := h.Close(); cerr != nil {
			t.Errorf("Handler.Close() returned %v, want nil", cerr)
		}
	})

	logger := slog.New(h)
	logger.DebugContext(context.Background(), "debug skipped")
	if buf.Len() != 0 {
		t.Fatalf("expected no log entries before lowering level, got %q", buf.String())
	}

	h.SetLevel(slog.LevelDebug)
	logger.DebugContext(context.Background(), "debug enabled", slog.String("k", "v"))

	entries := decodeLogBuffer(t, &buf)
	if len(entries) != 1 {
		t.Fatalf("expected 1 log entry after lowering level, got %d (%v)", len(entries), entries)
	}
	if got := entries[0]["message"]; got != "debug enabled" {
		t.Fatalf("message = %v, want debug enabled", got)
	}
	if got := entries[0]["k"]; got != "v" {
		t.Fatalf("attribute k = %v, want v", got)
	}
	if got := h.Level(); got != slog.LevelDebug {
		t.Fatalf("Level() = %v, want %v", got, slog.LevelDebug)
	}
}

// TestHandlerWithLevelVar ensures WithLevelVar shares a LevelVar instance.
func TestHandlerWithLevelVar(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	levelVar := new(slog.LevelVar)

	h, err := NewHandler(io.Discard, WithRedirectWriter(&buf), WithLevelVar(levelVar), WithLevel(slog.LevelWarn))
	if err != nil {
		t.Fatalf("NewHandler() returned %v, want nil", err)
	}
	t.Cleanup(func() {
		if cerr := h.Close(); cerr != nil {
			t.Errorf("Handler.Close() returned %v, want nil", cerr)
		}
	})

	if h.LevelVar() != levelVar {
		t.Fatalf("handler LevelVar pointer mismatch")
	}

	logger := slog.New(h)
	logger.InfoContext(context.Background(), "info suppressed")
	if buf.Len() != 0 {
		t.Fatalf("expected info log suppressed at warn level, got %q", buf.String())
	}

	levelVar.Set(slog.LevelInfo)
	logger.InfoContext(context.Background(), "info emitted")

	entries := decodeLogBuffer(t, &buf)
	if len(entries) != 1 {
		t.Fatalf("expected 1 log entry after lowering shared level, got %d (%v)", len(entries), entries)
	}
	if got := entries[0]["message"]; got != "info emitted" {
		t.Fatalf("message = %v, want info emitted", got)
	}
	if got := h.Level(); got != slog.LevelInfo {
		t.Fatalf("Level() = %v, want %v", got, slog.LevelInfo)
	}
}

// TestHandlerWithLevelVarSeedsFromEnv ensures env-driven configuration is applied
// even when a caller supplies their own LevelVar.
func TestHandlerWithLevelVarSeedsFromEnv(t *testing.T) {
	t.Run("defaultsToInfo", func(t *testing.T) {
		clearHandlerEnv(t)
		levelVar := new(slog.LevelVar)
		h, err := NewHandler(io.Discard, WithLevelVar(levelVar))
		if err != nil {
			t.Fatalf("NewHandler() returned %v, want nil", err)
		}
		t.Cleanup(func() {
			if cerr := h.Close(); cerr != nil {
				t.Errorf("Handler.Close() returned %v, want nil", cerr)
			}
		})

		if got := h.Level(); got != slog.LevelInfo {
			t.Fatalf("Level() = %v, want %v", got, slog.LevelInfo)
		}
		if got := levelVar.Level(); got != slog.LevelInfo {
			t.Fatalf("LevelVar.Level() = %v, want %v", got, slog.LevelInfo)
		}
	})

	t.Run("prefersSlogcpLevel", func(t *testing.T) {
		clearHandlerEnv(t)
		t.Setenv(envLogLevel, "critical")
		t.Setenv(envGenericLogLevel, "debug")
		levelVar := new(slog.LevelVar)
		h, err := NewHandler(io.Discard, WithLevelVar(levelVar))
		if err != nil {
			t.Fatalf("NewHandler() returned %v, want nil", err)
		}
		t.Cleanup(func() {
			if cerr := h.Close(); cerr != nil {
				t.Errorf("Handler.Close() returned %v, want nil", cerr)
			}
		})

		want := slog.Level(LevelCritical)
		if got := h.Level(); got != want {
			t.Fatalf("Level() = %v, want %v", got, want)
		}
		if got := levelVar.Level(); got != want {
			t.Fatalf("LevelVar.Level() = %v, want %v", got, want)
		}
	})

	t.Run("fallsBackToLogLevel", func(t *testing.T) {
		clearHandlerEnv(t)
		t.Setenv(envGenericLogLevel, "debug")
		levelVar := new(slog.LevelVar)
		h, err := NewHandler(io.Discard, WithLevelVar(levelVar))
		if err != nil {
			t.Fatalf("NewHandler() returned %v, want nil", err)
		}
		t.Cleanup(func() {
			if cerr := h.Close(); cerr != nil {
				t.Errorf("Handler.Close() returned %v, want nil", cerr)
			}
		})

		if got := h.Level(); got != slog.LevelDebug {
			t.Fatalf("Level() = %v, want %v", got, slog.LevelDebug)
		}
		if got := levelVar.Level(); got != slog.LevelDebug {
			t.Fatalf("LevelVar.Level() = %v, want %v", got, slog.LevelDebug)
		}
	})
}

// TestHandlerTimeFieldEmission verifies the handler omits the "time" field by
// default but can emit it when explicitly enabled.
func TestHandlerTimeFieldEmission(t *testing.T) {
	t.Setenv("SLOGCP_TIME", "")
	ResetHandlerConfigCacheForTest()
	ResetRuntimeInfoCacheForTest()

	t.Run("disabled_by_default", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer
		h, err := NewHandler(io.Discard, WithRedirectWriter(&buf))
		if err != nil {
			t.Fatalf("NewHandler() returned %v, want nil", err)
		}
		t.Cleanup(func() {
			if cerr := h.Close(); cerr != nil {
				t.Errorf("Handler.Close() returned %v, want nil", cerr)
			}
		})

		logger := slog.New(h)
		logger.InfoContext(context.Background(), "no-time")

		entries := decodeLogBuffer(t, &buf)
		if len(entries) != 1 {
			t.Fatalf("expected 1 log entry, got %d (%v)", len(entries), entries)
		}
		if prefersManagedDefaults() {
			if _, ok := entries[0]["time"]; ok {
				t.Fatalf("time field present despite managed-runtime default disablement: %v", entries[0])
			}
		} else {
			if _, ok := entries[0]["time"]; !ok {
				t.Fatalf("time field missing despite default enablement: %v", entries[0])
			}
		}
	})

	t.Run("enabled_via_option", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer
		h, err := NewHandler(io.Discard,
			WithRedirectWriter(&buf),
			WithTime(true),
		)
		if err != nil {
			t.Fatalf("NewHandler() returned %v, want nil", err)
		}
		t.Cleanup(func() {
			if cerr := h.Close(); cerr != nil {
				t.Errorf("Handler.Close() returned %v, want nil", cerr)
			}
		})

		logger := slog.New(h)
		logger.InfoContext(context.Background(), "time-enabled")

		entries := decodeLogBuffer(t, &buf)
		if len(entries) != 1 {
			t.Fatalf("expected 1 log entry, got %d (%v)", len(entries), entries)
		}
		rawTime, ok := entries[0]["time"].(string)
		if !ok || rawTime == "" {
			t.Fatalf("expected time string, got %v", entries[0]["time"])
		}
		if _, err := time.Parse(time.RFC3339Nano, rawTime); err != nil {
			t.Fatalf("time field %q is not RFC3339Nano: %v", rawTime, err)
		}
	})
}

// TestHandlerLevelAccessors ensures SetLevel/Level/LevelVar operate correctly.
func TestHandlerLevelAccessors(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	levelVar := new(slog.LevelVar)
	levelVar.Set(slog.LevelInfo)

	h, err := NewHandler(
		io.Discard,
		WithRedirectWriter(&buf),
		WithLevelVar(levelVar),
	)
	if err != nil {
		t.Fatalf("NewHandler() returned %v, want nil", err)
	}
	t.Cleanup(func() {
		if cerr := h.Close(); cerr != nil {
			t.Errorf("Handler.Close() returned %v, want nil", cerr)
		}
	})

	if got := h.Level(); got != slog.LevelInfo {
		t.Fatalf("initial Level() = %v, want info", got)
	}
	h.SetLevel(slog.LevelDebug)
	if got := h.Level(); got != slog.LevelDebug {
		t.Fatalf("Level() after SetLevel = %v, want debug", got)
	}
	if lv := h.LevelVar(); lv == nil || lv.Level() != slog.LevelDebug {
		t.Fatalf("LevelVar not synced, got %#v", lv)
	}
}

// TestHandlerHandleJSONEncodingFailure verifies encoding errors propagate and emit diagnostics.
func TestHandlerHandleJSONEncodingFailure(t *testing.T) {
	t.Parallel()

	var sink bytes.Buffer
	var diagBuf bytes.Buffer
	internalLogger := slog.New(slog.NewTextHandler(&diagBuf, &slog.HandlerOptions{AddSource: false}))

	h, err := NewHandler(
		io.Discard,
		WithRedirectWriter(&sink),
		WithInternalLogger(internalLogger),
	)
	if err != nil {
		t.Fatalf("NewHandler() returned %v", err)
	}
	t.Cleanup(func() {
		if cerr := h.Close(); cerr != nil {
			t.Errorf("Handler.Close() returned %v, want nil", cerr)
		}
	})

	var record slog.Record
	record.Time = time.Now()
	record.Level = slog.LevelInfo
	record.Message = "encode failure"
	record.AddAttrs(slog.Any("broken", failingJSONMarshaler{}))

	if err := h.Handle(context.Background(), record); err == nil {
		t.Fatalf("Handle() returned nil error, want encoding failure")
	}
	if sink.Len() != 0 {
		t.Fatalf("expected no log output, got %q", sink.String())
	}
	if !strings.Contains(diagBuf.String(), "failed to render JSON log entry") {
		t.Fatalf("diagnostic log missing encode failure, got %q", diagBuf.String())
	}
}

type failingJSONMarshaler struct{}

// MarshalJSON always fails to trigger handler encoding error paths.
func (failingJSONMarshaler) MarshalJSON() ([]byte, error) {
	return nil, errors.New("marshal failure")
}

// TestHandlerHandleWriterFailure verifies writer errors bubble up and trigger diagnostics.
func TestHandlerHandleWriterFailure(t *testing.T) {
	t.Parallel()

	var diagBuf bytes.Buffer
	internalLogger := slog.New(slog.NewTextHandler(&diagBuf, &slog.HandlerOptions{AddSource: false}))
	fail := &handlerFailingWriter{err: errors.New("sink down")}

	h, err := NewHandler(
		io.Discard,
		WithRedirectWriter(fail),
		WithInternalLogger(internalLogger),
	)
	if err != nil {
		t.Fatalf("NewHandler() returned %v, want nil", err)
	}
	t.Cleanup(func() {
		if cerr := h.Close(); cerr != nil {
			t.Errorf("Handler.Close() returned %v, want nil", cerr)
		}
	})

	var record slog.Record
	record.Time = time.Now()
	record.Level = slog.LevelInfo
	record.Message = "write failure"
	record.AddAttrs(slog.String("k", "v"))

	if err := h.Handle(context.Background(), record); err == nil {
		t.Fatalf("Handle() returned nil, want writer error")
	}
	if !strings.Contains(diagBuf.String(), "failed to write JSON log entry") {
		t.Fatalf("diagnostic log missing writer failure: %q", diagBuf.String())
	}
}

type handlerFailingWriter struct {
	err error
}

// Write always returns an error to simulate a broken sink.
func (f *handlerFailingWriter) Write([]byte) (int, error) {
	return 0, f.err
}

// TestSeverityHelpersEmitExpectedLevels exercises the severity-specific helpers to
// ensure they log at the intended slogcp levels and tolerate nil loggers.
func TestSeverityHelpersEmitExpectedLevels(t *testing.T) {
	t.Parallel()

	recorder := &severityRecordingHandler{}
	logger := slog.New(recorder)
	ctx := context.Background()

	DefaultContext(ctx, logger, "default")
	NoticeContext(ctx, logger, "notice")
	CriticalContext(ctx, logger, "critical")
	AlertContext(ctx, logger, "alert")
	EmergencyContext(ctx, logger, "emergency")

	// Ensure helpers do nothing when the logger is nil.
	AlertContext(ctx, nil, "ignored")

	want := []struct {
		msg   string
		level slog.Level
	}{
		{"default", slog.Level(LevelDefault)},
		{"notice", slog.Level(LevelNotice)},
		{"critical", slog.Level(LevelCritical)},
		{"alert", slog.Level(LevelAlert)},
		{"emergency", slog.Level(LevelEmergency)},
	}
	if len(recorder.records) != len(want) {
		t.Fatalf("got %d records, want %d", len(recorder.records), len(want))
	}
	for i, rec := range recorder.records {
		if rec.Message != want[i].msg {
			t.Fatalf("record %d message = %q, want %q", i, rec.Message, want[i].msg)
		}
		if rec.Level != want[i].level {
			t.Fatalf("record %d level = %v, want %v", i, rec.Level, want[i].level)
		}
	}
}

// severityRecordingHandler captures slog records for inspection in tests.
type severityRecordingHandler struct {
	records []slog.Record
}

// Enabled allows all records through during testing.
func (h *severityRecordingHandler) Enabled(context.Context, slog.Level) bool { return true }

// Handle records log entries for later assertions.
func (h *severityRecordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.records = append(h.records, r.Clone())
	return nil
}

// WithAttrs satisfies slog.Handler while keeping the recorder as-is.
func (h *severityRecordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

// WithGroup satisfies slog.Handler while keeping the recorder as-is.
func (h *severityRecordingHandler) WithGroup(string) slog.Handler { return h }

// handlerRecordingHandler captures the last record and attributes for helper tests.
type handlerRecordingHandler struct {
	lastRecord *slog.Record
	attrs      []slog.Attr
}

// Enabled always returns true for handlerRecordingHandler.
func (h *handlerRecordingHandler) Enabled(context.Context, slog.Level) bool { return true }

// Handle stores the record and its attributes.
func (h *handlerRecordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.lastRecord = &slog.Record{}
	*h.lastRecord = r
	h.attrs = make([]slog.Attr, 0, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		h.attrs = append(h.attrs, a)
		return true
	})
	return nil
}

// WithAttrs clones the handler with additional attributes.
func (h *handlerRecordingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &handlerRecordingHandler{lastRecord: h.lastRecord, attrs: append(append([]slog.Attr(nil), h.attrs...), attrs...)}
}

// WithGroup returns the handler unchanged because grouping is unused here.
func (h *handlerRecordingHandler) WithGroup(string) slog.Handler { return h }

// TestHandlerStackTraceLevelEmitsStacks verifies the stack trace level option triggers capture.
func TestHandlerStackTraceLevelEmitsStacks(t *testing.T) {
	ResetHandlerConfigCacheForTest()
	ResetRuntimeInfoCacheForTest()
	t.Setenv("SLOGCP_TARGET", "")

	var buf bytes.Buffer
	h, err := NewHandler(&buf,
		WithStackTraceEnabled(true),
		WithStackTraceLevel(slog.LevelWarn),
	)
	if err != nil {
		t.Fatalf("NewHandler() returned %v, want nil", err)
	}
	t.Cleanup(func() {
		if cerr := h.Close(); cerr != nil {
			t.Errorf("Handler.Close() returned %v, want nil", cerr)
		}
	})

	slog.New(h).Warn("stack please")

	entries := decodeLogBuffer(t, &buf)
	if len(entries) != 1 {
		t.Fatalf("expected 1 log entry, got %d", len(entries))
	}
	stack, _ := entries[0]["stack_trace"].(string)
	if stack == "" {
		t.Fatalf("stack_trace missing from warn entry: %#v", entries[0])
	}
}

// TestHandlerDefaultSeverityDoesNotTriggerStack ensures LevelDefault does not
// outrank error thresholds for stack trace capture.
func TestHandlerDefaultSeverityDoesNotTriggerStack(t *testing.T) {
	ResetHandlerConfigCacheForTest()
	ResetRuntimeInfoCacheForTest()
	t.Setenv("SLOGCP_TARGET", "")

	var buf bytes.Buffer
	h, err := NewHandler(&buf,
		WithStackTraceEnabled(true),
		WithStackTraceLevel(slog.LevelError),
	)
	if err != nil {
		t.Fatalf("NewHandler() returned %v, want nil", err)
	}
	t.Cleanup(func() {
		if cerr := h.Close(); cerr != nil {
			t.Errorf("Handler.Close() returned %v, want nil", cerr)
		}
	})

	logger := slog.New(h)
	logger.Log(context.Background(), slog.Level(LevelDefault), "default-severity")

	entries := decodeLogBuffer(t, &buf)
	if len(entries) != 1 {
		t.Fatalf("expected 1 log entry, got %d (%v)", len(entries), entries)
	}
	if _, ok := entries[0]["stack_trace"]; ok {
		t.Fatalf("stack_trace present for LevelDefault entry: %#v", entries[0])
	}
}

// TestHandlerMiddlewareInvokesHooks ensures WithMiddleware attaches middleware correctly.
func TestHandlerMiddlewareInvokesHooks(t *testing.T) {
	ResetHandlerConfigCacheForTest()
	ResetRuntimeInfoCacheForTest()
	t.Setenv("SLOGCP_TARGET", "")

	var buf bytes.Buffer
	middlewareCalled := false
	middleware := func(next slog.Handler) slog.Handler {
		return middlewareCaptureHandler{
			next: next,
			onHandle: func(r *slog.Record) {
				middlewareCalled = true
				r.AddAttrs(slog.String("middleware", "wrapped"))
			},
		}
	}

	h, err := NewHandler(&buf, WithMiddleware(middleware))
	if err != nil {
		t.Fatalf("NewHandler() returned %v, want nil", err)
	}
	t.Cleanup(func() {
		if cerr := h.Close(); cerr != nil {
			t.Errorf("Handler.Close() returned %v, want nil", cerr)
		}
	})

	slog.New(h).Info("middleware test")

	if !middlewareCalled {
		t.Fatalf("middleware hook not invoked")
	}

	entries := decodeLogBuffer(t, &buf)
	if len(entries) != 1 {
		t.Fatalf("expected 1 log entry, got %d", len(entries))
	}
	if got := entries[0]["middleware"]; got != "wrapped" {
		t.Fatalf("middleware attribute = %v, want wrapped", got)
	}
}

// TestWithRedirectToStdStreams validates WithRedirectToStdout and WithRedirectToStderr writers.
func TestWithRedirectToStdStreams(t *testing.T) {
	t.Run("stdout", func(t *testing.T) {
		reader, restore := captureStdStream(t, "stdout")
		restored := false
		defer func() {
			if !restored {
				restore()
			}
			if err := reader.Close(); err != nil {
				t.Fatalf("reader.Close() returned %v", err)
			}
		}()

		h, err := NewHandler(io.Discard, WithRedirectToStdout(), WithSeverityAliases(false))
		if err != nil {
			t.Fatalf("NewHandler() returned %v, want nil", err)
		}
		logger := slog.New(h)
		logger.Info("stdout redirect", slog.String("stream", "stdout"))
		if cerr := h.Close(); cerr != nil {
			t.Fatalf("Handler.Close() returned %v, want nil", cerr)
		}

		restore()
		restored = true
		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("ReadAll(stdout) returned %v, want nil", err)
		}
		if !strings.Contains(string(data), `"message":"stdout redirect"`) {
			t.Fatalf("stdout logs missing message: %q", data)
		}
	})

	t.Run("stderr", func(t *testing.T) {
		reader, restore := captureStdStream(t, "stderr")
		restored := false
		defer func() {
			if !restored {
				restore()
			}
			if err := reader.Close(); err != nil {
				t.Fatalf("reader.Close() returned %v", err)
			}
		}()

		h, err := NewHandler(io.Discard, WithRedirectToStderr(), WithSeverityAliases(false))
		if err != nil {
			t.Fatalf("NewHandler() returned %v, want nil", err)
		}
		logger := slog.New(h)
		logger.Error("stderr redirect", slog.String("stream", "stderr"))
		if cerr := h.Close(); cerr != nil {
			t.Fatalf("Handler.Close() returned %v, want nil", cerr)
		}

		restore()
		restored = true
		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("ReadAll(stderr) returned %v, want nil", err)
		}
		if !strings.Contains(string(data), `"message":"stderr redirect"`) {
			t.Fatalf("stderr logs missing message: %q", data)
		}
	})
}

// middlewareCaptureHandler wraps slog.Handlers to observe and transform records.
type middlewareCaptureHandler struct {
	next     slog.Handler
	onHandle func(*slog.Record)
}

// Enabled delegates level checks to the wrapped handler.
func (m middlewareCaptureHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return m.next.Enabled(ctx, level)
}

// Handle clones the record, invokes onHandle, then forwards to the next handler.
func (m middlewareCaptureHandler) Handle(ctx context.Context, r slog.Record) error {
	clone := r.Clone()
	if m.onHandle != nil {
		m.onHandle(&clone)
	}
	return m.next.Handle(ctx, clone)
}

// WithAttrs propagates attribute scopes through the middleware.
func (m middlewareCaptureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return middlewareCaptureHandler{
		next:     m.next.WithAttrs(attrs),
		onHandle: m.onHandle,
	}
}

// WithGroup propagates group scopes through the middleware.
func (m middlewareCaptureHandler) WithGroup(name string) slog.Handler {
	return middlewareCaptureHandler{
		next:     m.next.WithGroup(name),
		onHandle: m.onHandle,
	}
}

// captureStdStream routes stdout or stderr through a pipe for assertions.
func captureStdStream(t *testing.T, target string) (*os.File, func()) {
	t.Helper()

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() returned %v, want nil", err)
	}

	switch target {
	case "stdout":
		prev := os.Stdout
		os.Stdout = writer
		return reader, func() {
			if err := writer.Close(); err != nil {
				t.Fatalf("writer.Close() returned %v", err)
			}
			os.Stdout = prev
		}
	case "stderr":
		prev := os.Stderr
		os.Stderr = writer
		return reader, func() {
			if err := writer.Close(); err != nil {
				t.Fatalf("writer.Close() returned %v", err)
			}
			os.Stderr = prev
		}
	default:
		t.Fatalf("captureStdStream: unknown target %q", target)
		return nil, func() {}
	}
}

// TestHandlerNilReceiversSafe ensures nil handler methods are no-ops.
func TestHandlerNilReceiversSafe(t *testing.T) {
	t.Parallel()

	var h *Handler
	h.SetLevel(slog.LevelDebug)
	if got := h.Level(); got != slog.LevelInfo {
		t.Fatalf("Level() on nil handler = %v, want %v", got, slog.LevelInfo)
	}
	if h.LevelVar() != nil {
		t.Fatalf("LevelVar() on nil handler should be nil")
	}
}

// TestSeverityHelperFunctions validates severity helper methods emit expected levels.
func TestSeverityHelperFunctions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		call func(*slog.Logger)
		want slog.Level
	}{
		{
			name: "default",
			call: func(l *slog.Logger) { Default(l, "default event") },
			want: slog.Level(LevelDefault),
		},
		{
			name: "default_context",
			call: func(l *slog.Logger) { DefaultContext(context.Background(), l, "ctx default") },
			want: slog.Level(LevelDefault),
		},
		{
			name: "notice",
			call: func(l *slog.Logger) { NoticeContext(context.Background(), l, "notice") },
			want: slog.Level(LevelNotice),
		},
		{
			name: "critical",
			call: func(l *slog.Logger) { CriticalContext(context.Background(), l, "crit") },
			want: slog.Level(LevelCritical),
		},
		{
			name: "alert",
			call: func(l *slog.Logger) { AlertContext(context.Background(), l, "alert") },
			want: slog.Level(LevelAlert),
		},
		{
			name: "emergency",
			call: func(l *slog.Logger) { EmergencyContext(context.Background(), l, "emerg") },
			want: slog.Level(LevelEmergency),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := &handlerRecordingHandler{}
			logger := slog.New(recorder)
			tt.call(logger)
			if recorder.lastRecord == nil {
				t.Fatalf("no log entry captured")
			}
			if recorder.lastRecord.Level != tt.want {
				t.Fatalf("level = %v, want %v", recorder.lastRecord.Level, tt.want)
			}
		})
	}
}

// TestSeverityHelpersHandleNilLogger confirms helpers guard nil loggers.
func TestSeverityHelpersHandleNilLogger(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	Default(nil, "noop")
	DefaultContext(ctx, nil, "noop")
	NoticeContext(ctx, nil, "noop")
	CriticalContext(ctx, nil, "noop")
	AlertContext(ctx, nil, "noop")
	EmergencyContext(ctx, nil, "noop")
}

// newDiscardLogger returns a test logger that discards all output.
func newDiscardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// clearHandlerEnv resets the environment variables that influence handler configuration.
func clearHandlerEnv(t *testing.T) {
	t.Helper()
	t.Setenv(envLogLevel, "")
	t.Setenv(envGenericLogLevel, "")
	t.Setenv(envLogSource, "")
	t.Setenv(envLogTime, "")
	t.Setenv(envLogStackEnabled, "")
	t.Setenv(envLogStackLevel, "")
	t.Setenv(envSeverityAliases, "")
	t.Setenv(envTraceProjectID, "")
	t.Setenv(envProjectID, "")
	t.Setenv(envSlogcpGCP, "")
	t.Setenv(envGoogleProject, "")
	t.Setenv(envTarget, "")
	resetRuntimeInfoCache()
	resetHandlerConfigCache()
}

// TestWithAsyncWrapsHandler ensures WithAsync applies the async wrapper.
func TestWithAsyncWrapsHandler(t *testing.T) {
	h, err := NewHandler(io.Discard, WithAsync())
	if err != nil {
		t.Fatalf("NewHandler returned %v", err)
	}
	if _, ok := h.Handler.(*slogcpasync.Handler); !ok {
		t.Fatalf("Handler is %T, want *slogcpasync.Handler", h.Handler)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close returned %v", err)
	}
}

// TestFileTargetsAsyncByDefault verifies file targets queue writes without requiring WithAsync.
func TestFileTargetsAsyncByDefault(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "log.json")
	fileHandler, err := NewHandler(nil, WithRedirectToFile(logPath))
	if err != nil {
		t.Fatalf("file NewHandler returned %v", err)
	}
	if _, ok := fileHandler.Handler.(*slogcpasync.Handler); !ok {
		t.Fatalf("file Handler is %T, want *slogcpasync.Handler", fileHandler.Handler)
	}
	if err := fileHandler.Close(); err != nil {
		t.Fatalf("file Close returned %v", err)
	}

	stdHandler, err := NewHandler(io.Discard)
	if err != nil {
		t.Fatalf("stdout NewHandler returned %v", err)
	}
	if _, ok := stdHandler.Handler.(*slogcpasync.Handler); ok {
		t.Fatalf("stdout Handler unexpectedly async-wrapped")
	}
	if err := stdHandler.Close(); err != nil {
		t.Fatalf("stdout Close returned %v", err)
	}
}

// TestWithAsyncOnFileTargetsWrapsOnlyFiles applies async to file targets only.
func TestWithAsyncOnFileTargetsWrapsOnlyFiles(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "log.json")

	fileHandler, err := NewHandler(nil, WithRedirectToFile(logPath), WithAsyncOnFileTargets())
	if err != nil {
		t.Fatalf("file NewHandler returned %v", err)
	}
	if _, ok := fileHandler.Handler.(*slogcpasync.Handler); !ok {
		t.Fatalf("file Handler is %T, want *slogcpasync.Handler", fileHandler.Handler)
	}
	if err := fileHandler.Close(); err != nil {
		t.Fatalf("file Close returned %v", err)
	}

	stdHandler, err := NewHandler(io.Discard, WithAsyncOnFileTargets())
	if err != nil {
		t.Fatalf("stdout NewHandler returned %v", err)
	}
	if _, ok := stdHandler.Handler.(*slogcpasync.Handler); ok {
		t.Fatalf("stdout Handler unexpectedly async-wrapped")
	}
	if err := stdHandler.Close(); err != nil {
		t.Fatalf("stdout Close returned %v", err)
	}
}

// TestLoadConfigFromEnvLevelOverride verifies environment overrides adjust the minimum level.
func TestLoadConfigFromEnvLevelOverride(t *testing.T) {
	clearHandlerEnv(t)
	t.Setenv(envLogLevel, "warning")

	cfg, err := loadConfigFromEnv(newDiscardLogger())
	if err != nil {
		t.Fatalf("loadConfigFromEnv() returned %v, want nil", err)
	}
	if cfg.Level != slog.LevelWarn {
		t.Fatalf("cfg.Level = %v, want %v", cfg.Level, slog.LevelWarn)
	}
}

// TestLoadConfigFromEnvLevelFallback exercises LOG_LEVEL when SLOGCP_LEVEL is unset.
func TestLoadConfigFromEnvLevelFallback(t *testing.T) {
	clearHandlerEnv(t)
	t.Setenv(envGenericLogLevel, "alert")

	cfg, err := loadConfigFromEnv(newDiscardLogger())
	if err != nil {
		t.Fatalf("loadConfigFromEnv() returned %v, want nil", err)
	}
	if cfg.Level != slog.Level(LevelAlert) {
		t.Fatalf("cfg.Level = %v, want %v", cfg.Level, LevelAlert)
	}
}

// TestLoadConfigFromEnvLevelPrefersSlogcp ensures SLOGCP_LEVEL wins over LOG_LEVEL.
func TestLoadConfigFromEnvLevelPrefersSlogcp(t *testing.T) {
	clearHandlerEnv(t)
	t.Setenv(envGenericLogLevel, "debug")
	t.Setenv(envLogLevel, "error")

	cfg, err := loadConfigFromEnv(newDiscardLogger())
	if err != nil {
		t.Fatalf("loadConfigFromEnv() returned %v, want nil", err)
	}
	if cfg.Level != slog.LevelError {
		t.Fatalf("cfg.Level = %v, want %v", cfg.Level, slog.LevelError)
	}
}

// TestLoadConfigFromEnvBoolFlags ensures boolean environment variables are interpreted correctly.
func TestLoadConfigFromEnvBoolFlags(t *testing.T) {
	tests := []struct {
		name   string
		envVar string
		value  string
		assert func(t *testing.T, cfg handlerConfig)
	}{
		{
			name:   "source_location",
			envVar: envLogSource,
			value:  "true",
			assert: func(t *testing.T, cfg handlerConfig) {
				if !cfg.AddSource {
					t.Fatalf("cfg.AddSource = false, want true")
				}
			},
		},
		{
			name:   "time_field",
			envVar: envLogTime,
			value:  "1",
			assert: func(t *testing.T, cfg handlerConfig) {
				if !cfg.EmitTimeField {
					t.Fatalf("cfg.EmitTimeField = false, want true")
				}
			},
		},
		{
			name:   "stack_trace_enabled",
			envVar: envLogStackEnabled,
			value:  "true",
			assert: func(t *testing.T, cfg handlerConfig) {
				if !cfg.StackTraceEnabled {
					t.Fatalf("cfg.StackTraceEnabled = false, want true")
				}
			},
		},
		{
			name:   "severity_aliases",
			envVar: envSeverityAliases,
			value:  "false",
			assert: func(t *testing.T, cfg handlerConfig) {
				if cfg.UseShortSeverityNames {
					t.Fatalf("cfg.UseShortSeverityNames = true, want false")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearHandlerEnv(t)
			t.Setenv(tt.envVar, tt.value)

			cfg, err := loadConfigFromEnv(newDiscardLogger())
			if err != nil {
				t.Fatalf("loadConfigFromEnv() returned %v, want nil", err)
			}
			tt.assert(t, cfg)
		})
	}
}

// TestLoadConfigFromEnvSeverityAliasDefaultNonGCP ensures aliases are disabled by default outside GCP.
func TestLoadConfigFromEnvSeverityAliasDefaultNonGCP(t *testing.T) {
	clearHandlerEnv(t)

	cfg, err := loadConfigFromEnv(newDiscardLogger())
	if err != nil {
		t.Fatalf("loadConfigFromEnv() returned %v, want nil", err)
	}
	if cfg.UseShortSeverityNames {
		t.Fatalf("cfg.UseShortSeverityNames = true, want false when no GCP runtime detected")
	}
}

// TestLoadConfigFromEnvSeverityAliasDefaultGCP ensures aliases stay enabled by default when GCP is detected.
func TestLoadConfigFromEnvSeverityAliasDefaultGCP(t *testing.T) {
	clearHandlerEnv(t)
	t.Setenv("K_SERVICE", "svc")
	t.Setenv("K_REVISION", "rev")
	t.Setenv("K_CONFIGURATION", "cfg")
	t.Setenv(envGoogleProject, "project-id")

	cfg, err := loadConfigFromEnv(newDiscardLogger())
	if err != nil {
		t.Fatalf("loadConfigFromEnv() returned %v, want nil", err)
	}
	if !cfg.UseShortSeverityNames {
		t.Fatalf("cfg.UseShortSeverityNames = false, want true when GCP runtime detected")
	}
}

// TestLoadConfigFromEnvTimeDefaultNonGCP ensures timestamps are emitted by default outside managed GCP runtimes.
func TestLoadConfigFromEnvTimeDefaultNonGCP(t *testing.T) {
	clearHandlerEnv(t)

	cfg, err := loadConfigFromEnv(newDiscardLogger())
	if err != nil {
		t.Fatalf("loadConfigFromEnv() returned %v, want nil", err)
	}
	if !cfg.EmitTimeField {
		t.Fatalf("cfg.EmitTimeField = false, want true outside managed GCP runtimes")
	}
}

// TestLoadConfigFromEnvTimeDefaultCloudRun ensures timestamps are omitted by default on Cloud Run.
func TestLoadConfigFromEnvTimeDefaultCloudRun(t *testing.T) {
	clearHandlerEnv(t)
	t.Setenv("K_SERVICE", "svc")
	t.Setenv("K_REVISION", "rev")
	t.Setenv("K_CONFIGURATION", "cfg")

	cfg, err := loadConfigFromEnv(newDiscardLogger())
	if err != nil {
		t.Fatalf("loadConfigFromEnv() returned %v, want nil", err)
	}
	if cfg.EmitTimeField {
		t.Fatalf("cfg.EmitTimeField = true, want false on Cloud Run by default")
	}
}

// TestFileTargetDefaultsToTimeOnManagedGCP ensures file targets keep timestamps even on managed runtimes.
func TestFileTargetDefaultsToTimeOnManagedGCP(t *testing.T) {
	clearHandlerEnv(t)
	t.Setenv("K_SERVICE", "svc")
	t.Setenv("K_REVISION", "rev")
	t.Setenv("K_CONFIGURATION", "cfg")
	logPath := filepath.Join(t.TempDir(), "file.json")

	h, err := NewHandler(nil, WithRedirectToFile(logPath))
	if err != nil {
		t.Fatalf("NewHandler returned %v", err)
	}
	t.Cleanup(func() {
		if cerr := h.Close(); cerr != nil {
			t.Errorf("Close returned %v", cerr)
		}
	})
	if !h.cfg.EmitTimeField {
		t.Fatalf("EmitTimeField = false, want true for file target on managed GCP runtimes")
	}
}

// TestRedirectWriterFileDefaultsToTime ensures externally provided file writers keep timestamps.
func TestRedirectWriterFileDefaultsToTime(t *testing.T) {
	clearHandlerEnv(t)
	t.Setenv("K_SERVICE", "svc")
	t.Setenv("K_REVISION", "rev")
	t.Setenv("K_CONFIGURATION", "cfg")
	logPath := filepath.Join(t.TempDir(), "file.json")
	file, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("os.Create(%q) returned %v", logPath, err)
	}
	t.Cleanup(func() {
		_ = file.Close()
	})

	h, err := NewHandler(nil, WithRedirectWriter(file))
	if err != nil {
		t.Fatalf("NewHandler returned %v", err)
	}
	t.Cleanup(func() {
		if cerr := h.Close(); cerr != nil {
			t.Errorf("Close returned %v", cerr)
		}
	})
	if !h.cfg.EmitTimeField {
		t.Fatalf("EmitTimeField = false, want true for explicit file writer targets")
	}
}

// TestFileTargetHonorsExplicitTimeDisable ensures file targets respect explicit WithTime(false) overrides.
func TestFileTargetHonorsExplicitTimeDisable(t *testing.T) {
	clearHandlerEnv(t)
	t.Setenv("K_SERVICE", "svc")
	t.Setenv("K_REVISION", "rev")
	t.Setenv("K_CONFIGURATION", "cfg")
	logPath := filepath.Join(t.TempDir(), "file.json")

	h, err := NewHandler(nil, WithRedirectToFile(logPath), WithTime(false))
	if err != nil {
		t.Fatalf("NewHandler returned %v", err)
	}
	t.Cleanup(func() {
		if cerr := h.Close(); cerr != nil {
			t.Errorf("Close returned %v", cerr)
		}
	})
	if h.cfg.EmitTimeField {
		t.Fatalf("EmitTimeField = true, want false when WithTime(false) is provided for file targets")
	}
}

// TestHasFileTargetNilConfig ensures a nil handler config is treated as having no file target.
func TestHasFileTargetNilConfig(t *testing.T) {
	if hasFileTarget(nil) {
		t.Fatalf("hasFileTarget(nil) = true, want false")
	}
}

// TestWriterIsFileTarget exercises edge cases around nested SwitchableWriter plumbing.
func TestWriterIsFileTarget(t *testing.T) {
	t.Run("nilWriter", func(t *testing.T) {
		if writerIsFileTarget(nil, 0) {
			t.Fatalf("writerIsFileTarget(nil, 0) = true, want false")
		}
	})

	t.Run("switchableWriterDelegatesToFile", func(t *testing.T) {
		logPath := filepath.Join(t.TempDir(), "writer.json")
		file, err := os.Create(logPath)
		if err != nil {
			t.Fatalf("os.Create(%q) returned %v", logPath, err)
		}
		t.Cleanup(func() {
			_ = file.Close()
		})

		sw := NewSwitchableWriter(file)
		if !writerIsFileTarget(sw, 0) {
			t.Fatalf("writerIsFileTarget() = false, want true for switchable writer backed by file")
		}
	})

	t.Run("switchableWriterCycle", func(t *testing.T) {
		sw := NewSwitchableWriter(nil)
		sw.SetWriter(sw)

		if writerIsFileTarget(sw, 0) {
			t.Fatalf("writerIsFileTarget() = true, want false when SwitchableWriter returns itself")
		}
	})
}

// TestLoadConfigFromEnvStackTraceLevel confirms the stack trace level override is applied.
func TestLoadConfigFromEnvStackTraceLevel(t *testing.T) {
	clearHandlerEnv(t)
	t.Setenv(envLogStackLevel, "notice")

	cfg, err := loadConfigFromEnv(newDiscardLogger())
	if err != nil {
		t.Fatalf("loadConfigFromEnv() returned %v, want nil", err)
	}
	if cfg.StackTraceLevel != slog.Level(LevelNotice) {
		t.Fatalf("cfg.StackTraceLevel = %v, want %v", cfg.StackTraceLevel, slog.Level(LevelNotice))
	}
}

// TestLoadConfigFromEnvTargets exercises environment target resolution and validation.
func TestLoadConfigFromEnvTargets(t *testing.T) {
	t.Run("stdout", func(t *testing.T) {
		clearHandlerEnv(t)
		t.Setenv(envTarget, "stdout")

		cfg, err := loadConfigFromEnv(newDiscardLogger())
		if err != nil {
			t.Fatalf("loadConfigFromEnv() returned %v, want nil", err)
		}
		if cfg.Writer != os.Stdout {
			t.Fatalf("cfg.Writer = %v, want os.Stdout", cfg.Writer)
		}
		if !cfg.writerExternallyOwned {
			t.Fatalf("cfg.writerExternallyOwned = false, want true")
		}
	})

	t.Run("stderr", func(t *testing.T) {
		clearHandlerEnv(t)
		t.Setenv(envTarget, "stderr")

		cfg, err := loadConfigFromEnv(newDiscardLogger())
		if err != nil {
			t.Fatalf("loadConfigFromEnv() returned %v, want nil", err)
		}
		if cfg.Writer != os.Stderr {
			t.Fatalf("cfg.Writer = %v, want os.Stderr", cfg.Writer)
		}
		if !cfg.writerExternallyOwned {
			t.Fatalf("cfg.writerExternallyOwned = false, want true")
		}
	})

	t.Run("file", func(t *testing.T) {
		clearHandlerEnv(t)
		path := filepath.Join(t.TempDir(), "app.log")
		t.Setenv(envTarget, "file:"+path)

		cfg, err := loadConfigFromEnv(newDiscardLogger())
		if err != nil {
			t.Fatalf("loadConfigFromEnv() returned %v, want nil", err)
		}
		if cfg.FilePath != path {
			t.Fatalf("cfg.FilePath = %q, want %q", cfg.FilePath, path)
		}
		if cfg.Writer != nil {
			t.Fatalf("cfg.Writer = %v, want nil", cfg.Writer)
		}
		if cfg.writerExternallyOwned {
			t.Fatalf("cfg.writerExternallyOwned = true, want false")
		}
	})

	t.Run("invalid", func(t *testing.T) {
		clearHandlerEnv(t)
		t.Setenv(envTarget, "unknown")

		if _, err := loadConfigFromEnv(newDiscardLogger()); err == nil {
			t.Fatalf("loadConfigFromEnv() error = nil, want ErrInvalidRedirectTarget")
		}
	})

	t.Run("empty_file", func(t *testing.T) {
		clearHandlerEnv(t)
		t.Setenv(envTarget, "file:")

		if _, err := loadConfigFromEnv(newDiscardLogger()); err == nil {
			t.Fatalf("loadConfigFromEnv() error = nil, want ErrInvalidRedirectTarget")
		}
	})
}

// TestParseBoolEnvCoversValidation exercises valid, invalid, and blank values.
func TestParseBoolEnvCoversValidation(t *testing.T) {
	t.Parallel()

	logger := newDiscardLogger()
	if !parseBoolEnv("true", false, logger) {
		t.Fatalf("parseBoolEnv(true) = false, want true")
	}
	if parseBoolEnv("invalid", true, logger) != true {
		t.Fatalf("parseBoolEnv(invalid) should keep current value")
	}
	if parseBoolEnv("", true, logger) != true {
		t.Fatalf("parseBoolEnv blank should keep current value")
	}
}

// TestParseLevelEnvCoversAliases exercises numeric and named inputs.
func TestParseLevelEnvCoversAliases(t *testing.T) {
	t.Parallel()

	logger := newDiscardLogger()
	tests := []struct {
		input string
		want  slog.Level
	}{
		{input: "info", want: slog.LevelInfo},
		{input: "warn", want: slog.LevelWarn},
		{input: "warning", want: slog.LevelWarn},
		{input: "error", want: slog.LevelError},
		{input: "default", want: slog.Level(LevelDefault)},
		{input: "debug", want: slog.LevelDebug},
		{input: "NOTICE", want: slog.Level(LevelNotice)},
		{input: "critical", want: slog.Level(LevelCritical)},
		{input: "alert", want: slog.Level(LevelAlert)},
		{input: "emergency", want: slog.Level(LevelEmergency)},
		{input: "17", want: slog.Level(17)},
	}
	for _, tt := range tests {
		if got := parseLevelEnv(tt.input, slog.LevelInfo, logger); got != tt.want {
			t.Fatalf("parseLevelEnv(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}

	if got := parseLevelEnv("invalid", slog.LevelWarn, logger); got != slog.LevelWarn {
		t.Fatalf("parseLevelEnv invalid should retain current level")
	}
}

// TestCachedConfigFromEnvCachesResults ensures the first successful load is reused.
func TestCachedConfigFromEnvCachesResults(t *testing.T) {
	clearHandlerEnv(t)
	t.Cleanup(resetHandlerConfigCache)

	t.Setenv(envLogLevel, "error")
	cfg, err := cachedConfigFromEnv(newDiscardLogger())
	if err != nil {
		t.Fatalf("cachedConfigFromEnv() returned %v", err)
	}
	if cfg.Level != slog.LevelError {
		t.Fatalf("cached level = %v, want %v", cfg.Level, slog.LevelError)
	}

	// Changing the environment without resetting the cache should not affect the result.
	t.Setenv(envLogLevel, "debug")
	cfgAgain, err := cachedConfigFromEnv(newDiscardLogger())
	if err != nil {
		t.Fatalf("cachedConfigFromEnv() second call returned %v", err)
	}
	if cfgAgain.Level != slog.LevelError {
		t.Fatalf("cached level after env change = %v, want %v", cfgAgain.Level, slog.LevelError)
	}
}

// TestCachedConfigFromEnvReturnsLocalCfgWhenCacheClearedAfterCAS verifies the fallback return when CAS fails and the cache is cleared.
func TestCachedConfigFromEnvReturnsLocalCfgWhenCacheClearedAfterCAS(t *testing.T) {
	clearHandlerEnv(t)
	resetHandlerConfigCache()

	origLoader := getLoadConfigFromEnv()
	origHook := getCachedConfigRaceHook()
	t.Cleanup(func() {
		setLoadConfigFromEnv(origLoader)
		setCachedConfigRaceHook(origHook)
		resetHandlerConfigCache()
	})

	// Loader simulates a concurrent cache population before CompareAndSwap runs.
	setLoadConfigFromEnv(func(logger *slog.Logger) (handlerConfig, error) {
		handlerEnvConfigCache.Store(&handlerConfig{Level: slog.LevelWarn})
		return handlerConfig{Level: slog.LevelDebug}, nil
	})

	// Hook clears the cache so the function must return the locally loaded cfg.
	setCachedConfigRaceHook(func() {
		resetHandlerConfigCache()
	})

	cfg, err := cachedConfigFromEnv(newDiscardLogger())
	if err != nil {
		t.Fatalf("cachedConfigFromEnv() returned %v", err)
	}
	if cfg.Level != slog.LevelDebug {
		t.Fatalf("cfg.Level = %v, want %v (local cfg)", cfg.Level, slog.LevelDebug)
	}
	if cached := handlerEnvConfigCache.Load(); cached != nil {
		t.Fatalf("handlerEnvConfigCache should be cleared, got %+v", cached)
	}
}

// TestIsStdStream ensures stdout/stderr detection works and rejects other writers.
func TestIsStdStream(t *testing.T) {
	t.Parallel()

	if !isStdStream(os.Stdout) {
		t.Fatalf("expected stdout to be detected")
	}
	if !isStdStream(os.Stderr) {
		t.Fatalf("expected stderr to be detected")
	}
	if isStdStream(io.Discard) {
		t.Fatalf("io.Discard should not be treated as std stream")
	}
	var nilFile *os.File
	if isStdStream(nilFile) {
		t.Fatalf("nil *os.File should not be reported as std stream")
	}
}

// TestLogDiagnosticHandlesNilLogger ensures nil loggers are ignored.
func TestLogDiagnosticHandlesNilLogger(t *testing.T) {
	t.Parallel()

	logDiagnostic(nil, slog.LevelInfo, "message")

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{AddSource: false}))
	logDiagnostic(logger, slog.LevelInfo, "hello", slog.String("k", "v"))
	var payload map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &payload); err != nil {
		t.Fatalf("json.Unmarshal() returned %v", err)
	}
	if got := payload["msg"]; got != "hello" {
		t.Fatalf("payload msg = %v, want %q", got, "hello")
	}
}

// TestNewTraceDiagnosticsFallbacks ensures a default logger is created when the global logger is nil.
func TestNewTraceDiagnosticsFallbacks(t *testing.T) {
	original := traceDiagnosticLogger
	t.Cleanup(func() {
		traceDiagnosticLogger = original
	})
	traceDiagnosticLogger = nil

	if td := newTraceDiagnostics(TraceDiagnosticsOff); td != nil {
		t.Fatalf("newTraceDiagnostics(off) = %v, want nil", td)
	}

	td := newTraceDiagnostics(TraceDiagnosticsWarnOnce)
	if td == nil {
		t.Fatalf("newTraceDiagnostics returned nil in warn once mode")
	}
	if td.logger == nil {
		t.Fatalf("trace diagnostics logger should fall back to non-nil instance")
	}
}

// TestTraceDiagnosticsWarnUnknownProjectLogsOnce ensures warnings are emitted at most once per instance.
func TestTraceDiagnosticsWarnUnknownProjectLogsOnce(t *testing.T) {
	t.Parallel()

	logger := &diagRecorder{}
	td := &traceDiagnostics{mode: TraceDiagnosticsWarnOnce, logger: logger}
	td.warnUnknownProject()
	td.warnUnknownProject()

	if len(logger.messages) != 1 {
		t.Fatalf("warnUnknownProject logged %d messages, want 1", len(logger.messages))
	}

	silent := &traceDiagnostics{mode: TraceDiagnosticsOff, logger: logger}
	silent.warnUnknownProject()
	if len(logger.messages) != 1 {
		t.Fatalf("TraceDiagnosticsOff should not log warnings")
	}
}

// TestParseTraceDiagnosticsEnvCoversModes validates environment inputs and defaults.
func TestParseTraceDiagnosticsEnvCoversModes(t *testing.T) {
	t.Parallel()

	logger := newDiscardLogger()
	tests := []struct {
		name    string
		value   string
		current TraceDiagnosticsMode
		want    TraceDiagnosticsMode
	}{
		{"blank", "", TraceDiagnosticsWarnOnce, TraceDiagnosticsWarnOnce},
		{"off_alias", "disable", TraceDiagnosticsWarnOnce, TraceDiagnosticsOff},
		{"warn_alias", "warn-once", TraceDiagnosticsOff, TraceDiagnosticsWarnOnce},
		{"strict", "strict", TraceDiagnosticsWarnOnce, TraceDiagnosticsStrict},
		{"invalid", "mystery", TraceDiagnosticsStrict, TraceDiagnosticsStrict},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseTraceDiagnosticsEnv(tt.value, tt.current, logger); got != tt.want {
				t.Fatalf("parseTraceDiagnosticsEnv(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}

// TestLoadConfigFromEnvHelpersFallback exercises the helper accessors and defaulting paths.
func TestLoadConfigFromEnvHelpersFallback(t *testing.T) {
	originalLoader := getLoadConfigFromEnv()
	defer setLoadConfigFromEnv(originalLoader)

	setLoadConfigFromEnv(nil)
	if fn := getLoadConfigFromEnv(); fn == nil {
		t.Fatalf("getLoadConfigFromEnv returned nil after nil setter")
	}

	loadConfigFromEnvFunc = atomic.Value{}
	if fn := getLoadConfigFromEnv(); fn == nil {
		t.Fatalf("getLoadConfigFromEnv returned nil after zeroing atomic value")
	}

	setLoadConfigFromEnv(originalLoader)
}

// TestCachedConfigFromEnvInvokesRaceHook simulates a concurrent cache fill to ensure the hook fires.
func TestCachedConfigFromEnvInvokesRaceHook(t *testing.T) {
	clearHandlerEnv(t)
	resetHandlerConfigCache()

	originalLoader := getLoadConfigFromEnv()
	originalHook := getCachedConfigRaceHook()
	t.Cleanup(func() {
		setLoadConfigFromEnv(originalLoader)
		setCachedConfigRaceHook(originalHook)
		resetHandlerConfigCache()
	})

	raceWinner := &handlerConfig{Level: slog.LevelWarn}
	setLoadConfigFromEnv(func(logger *slog.Logger) (handlerConfig, error) {
		handlerEnvConfigCache.Store(raceWinner)
		return handlerConfig{Level: slog.LevelInfo}, nil
	})

	hookCalled := false
	setCachedConfigRaceHook(func() {
		hookCalled = true
	})

	cfg, err := cachedConfigFromEnv(newDiscardLogger())
	if err != nil {
		t.Fatalf("cachedConfigFromEnv() returned %v, want nil", err)
	}
	if !hookCalled {
		t.Fatalf("cachedConfigRaceHook was not invoked")
	}
	if cfg.Level != raceWinner.Level {
		t.Fatalf("cfg.Level = %v, want %v from existing cache", cfg.Level, raceWinner.Level)
	}
}

// diagRecorder records diagnostic messages for assertions.
type diagRecorder struct {
	messages []string
}

// Printf appends formatted messages for verification in tests.
func (l *diagRecorder) Printf(format string, args ...any) {
	l.messages = append(l.messages, fmt.Sprintf(format, args...))
}

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
	h, err := NewHandler(io.Discard, WithRedirectWriter(&buf))
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
	h, err := NewHandler(io.Discard, WithRedirectWriter(writer))
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
	h, err := NewHandler(io.Discard, WithRedirectWriter(writer))
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

type strictMetadataClient struct{}

// OnGCE reports that the strictMetadataClient is never running on GCE.
func (strictMetadataClient) OnGCE() bool { return false }

// Get always returns an error so diagnostics must rely on explicit project IDs.
func (strictMetadataClient) Get(string) (string, error) { return "", errors.New("unavailable") }

// TestNewHandlerStrictDiagnosticsRequiresProject ensures strict diagnostics fail fast when no project can be detected.
func TestNewHandlerStrictDiagnosticsRequiresProject(t *testing.T) {
	t.Setenv("SLOGCP_TRACE_PROJECT_ID", "")
	t.Setenv("SLOGCP_PROJECT_ID", "")
	t.Setenv("SLOGCP_GCP_PROJECT", "")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")
	t.Setenv("GCLOUD_PROJECT", "")
	t.Setenv("GCP_PROJECT", "")
	t.Setenv("PROJECT_ID", "")

	origFactory := getMetadataClientFactory()
	setMetadataClientFactory(func() metadataClient { return strictMetadataClient{} })
	t.Cleanup(func() { setMetadataClientFactory(origFactory) })

	resetRuntimeInfoCache()
	t.Cleanup(resetRuntimeInfoCache)

	handler, err := NewHandler(io.Discard, WithTraceDiagnostics(TraceDiagnosticsStrict))
	if err == nil {
		t.Fatalf("NewHandler() returned nil error, want strict mode failure")
	}
	if handler != nil {
		t.Fatalf("NewHandler() returned handler despite strict diagnostics failure")
	}
	if !strings.Contains(err.Error(), "requires a Cloud project ID") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestSourceAwareHandlerPropagatesSourceMetadata validates HasSource/With* wrappers.
func TestSourceAwareHandlerPropagatesSourceMetadata(t *testing.T) {
	t.Parallel()

	base := slog.NewJSONHandler(new(bytes.Buffer), &slog.HandlerOptions{AddSource: true})
	wrapped := sourceAwareHandler{Handler: base}

	if !wrapped.HasSource() {
		t.Fatalf("sourceAwareHandler.HasSource() = false, want true")
	}
	if _, ok := wrapped.WithAttrs(nil).(sourceAwareHandler); !ok {
		t.Fatalf("WithAttrs did not return sourceAwareHandler")
	}
	if _, ok := wrapped.WithGroup("grp").(sourceAwareHandler); !ok {
		t.Fatalf("WithGroup did not return sourceAwareHandler")
	}
}

// TestSourceAwareHandlerHandlesNilChildren confirms the wrapper tolerates nil child handlers.
func TestSourceAwareHandlerHandlesNilChildren(t *testing.T) {
	t.Parallel()

	wrapped := sourceAwareHandler{Handler: nilChildHandler{}}

	if wrapped.WithAttrs(nil) != nil {
		t.Fatalf("WithAttrs on nil child should return nil")
	}
	if wrapped.WithGroup("grp") != nil {
		t.Fatalf("WithGroup on nil child should return nil")
	}
}

// TestOptionHelpersMutateOptions ensures the option helpers toggle internal handler options.
func TestOptionHelpersMutateOptions(t *testing.T) {
	t.Parallel()

	t.Run("WithTime", func(t *testing.T) {
		var opts options
		WithTime(true)(&opts)
		if opts.emitTimeField == nil || !*opts.emitTimeField {
			t.Fatalf("emitTimeField = %v, want true", opts.emitTimeField)
		}
		WithTime(false)(&opts)
		if opts.emitTimeField == nil || *opts.emitTimeField {
			t.Fatalf("emitTimeField = %v, want false", opts.emitTimeField)
		}
	})

	t.Run("WithTraceProjectID", func(t *testing.T) {
		var opts options
		WithTraceProjectID("  proj-123  ")(&opts)
		if opts.traceProjectID == nil || *opts.traceProjectID != "proj-123" {
			t.Fatalf("traceProjectID = %v, want proj-123", opts.traceProjectID)
		}
	})

	t.Run("RedirectTargets", func(t *testing.T) {
		var opts options
		WithRedirectToStdout()(&opts)
		if opts.writer != os.Stdout || !opts.writerExternallyOwned {
			t.Fatalf("stdout writer not configured correctly: %+v", opts)
		}
		if opts.writerFilePath != nil {
			t.Fatalf("writerFilePath should be nil for stdout redirect")
		}

		WithRedirectToStderr()(&opts)
		if opts.writer != os.Stderr || !opts.writerExternallyOwned {
			t.Fatalf("stderr writer not configured correctly: %+v", opts)
		}

		WithRedirectToFile("  ./logs/app.json  ")(&opts)
		if opts.writerFilePath == nil || *opts.writerFilePath != "./logs/app.json" {
			t.Fatalf("writerFilePath = %v, want ./logs/app.json", opts.writerFilePath)
		}
		if opts.writer != nil || opts.writerExternallyOwned {
			t.Fatalf("writer should be nil and not externally owned after file redirect")
		}
	})

	t.Run("WithAttrsCopiesValues", func(t *testing.T) {
		attrs := []slog.Attr{slog.String("env", "prod")}
		var opts options
		WithAttrs(attrs)(&opts)
		if len(opts.attrs) != 1 || len(opts.attrs[0]) != 1 {
			t.Fatalf("attrs were not appended: %+v", opts.attrs)
		}
		attrs[0].Value = slog.StringValue("staging")
		if got := opts.attrs[0][0].Value.String(); got != "prod" {
			t.Fatalf("stored attribute mutated with input slice: got %q", got)
		}
	})

	t.Run("WithGroupTrimAndReset", func(t *testing.T) {
		var opts options
		opts.groups = []string{"initial"}

		WithGroup("  api  ")(&opts)
		if !opts.groupsSet {
			t.Fatalf("groupsSet should be true after WithGroup")
		}
		if want := []string{"initial", "api"}; len(opts.groups) != len(want) || opts.groups[1] != want[1] {
			t.Fatalf("groups not appended/trimmed: %+v", opts.groups)
		}

		WithGroup(" ")(&opts)
		if opts.groups != nil {
			t.Fatalf("blank group should reset groups slice, got %+v", opts.groups)
		}
	})
}

// TestNewHandlerPropagatesEnvErrors ensures invalid environment overrides bubble up to the caller.
func TestNewHandlerPropagatesEnvErrors(t *testing.T) {
	clearHandlerEnv(t)
	t.Setenv(envTarget, "invalid-target")

	if _, err := NewHandler(io.Discard); err == nil || !errors.Is(err, ErrInvalidRedirectTarget) {
		t.Fatalf("NewHandler() error = %v, want ErrInvalidRedirectTarget", err)
	}
}

// TestNewHandlerDefaultsToStdoutWriter verifies stdout is used when no writer or default is provided.
func TestNewHandlerDefaultsToStdoutWriter(t *testing.T) {
	clearHandlerEnv(t)

	h, err := NewHandler(nil)
	if err != nil {
		t.Fatalf("NewHandler(nil) returned %v, want nil", err)
	}
	t.Cleanup(func() {
		if cerr := h.Close(); cerr != nil {
			t.Errorf("Handler.Close() returned %v", cerr)
		}
	})

	if h.cfg.Writer != os.Stdout {
		t.Fatalf("cfg.Writer = %T, want os.Stdout", h.cfg.Writer)
	}
	if !h.cfg.writerExternallyOwned {
		t.Fatalf("writerExternallyOwned = false, want true for default stdout")
	}
}

// TestEnsureWriterFallbackSetsStdout verifies the fallback helper installs stdout.
func TestEnsureWriterFallbackSetsStdout(t *testing.T) {
	cfg := handlerConfig{FilePath: "/var/log/app.log"}
	ensureWriterFallback(&cfg)
	if cfg.Writer != os.Stdout {
		t.Fatalf("Writer = %v, want stdout", cfg.Writer)
	}
	if !cfg.writerExternallyOwned {
		t.Fatalf("writerExternallyOwned = false, want true")
	}
}

// TestNewHandlerFileOpenFailure surfaces file creation problems to the caller.
func TestNewHandlerFileOpenFailure(t *testing.T) {
	clearHandlerEnv(t)

	dir := t.TempDir()
	badPath := filepath.Join(dir, "missing", "app.log")

	_, err := NewHandler(io.Discard, WithRedirectToFile(badPath))
	if err == nil {
		t.Fatalf("NewHandler() error = nil, want failure opening %q", badPath)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("NewHandler() error = %v, want wrapping os.ErrNotExist", err)
	}
}

// TestNewHandlerWrapsSourceAwareHandler verifies handlers with AddSource=true expose HasSource.
func TestNewHandlerWrapsSourceAwareHandler(t *testing.T) {
	t.Parallel()

	h, err := NewHandler(io.Discard, WithSourceLocationEnabled(true))
	if err != nil {
		t.Fatalf("NewHandler() returned %v", err)
	}
	t.Cleanup(func() {
		if cerr := h.Close(); cerr != nil {
			t.Errorf("Handler.Close() returned %v", cerr)
		}
	})

	wrapper, ok := h.Handler.(sourceAwareHandler)
	if !ok {
		t.Fatalf("handler not wrapped with sourceAwareHandler: %T", h.Handler)
	}
	if !wrapper.HasSource() {
		t.Fatalf("HasSource() = false, want true")
	}

	child := wrapper.WithAttrs([]slog.Attr{slog.String("feature", "source-aware")})
	if _, ok := child.(sourceAwareHandler); !ok {
		t.Fatalf("WithAttrs did not retain source awareness: %T", child)
	}

	grouped := wrapper.WithGroup("nested")
	if _, ok := grouped.(sourceAwareHandler); !ok {
		t.Fatalf("WithGroup did not retain source awareness: %T", grouped)
	}
}

// TestHandlerCloseClosesOwnedResources exercises the branches that release switchable writers,
// owned files, and configured closers.
func TestHandlerCloseClosesOwnedResources(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "app.log")
	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("os.OpenFile(%q) = %v", logPath, err)
	}

	switchWriter := &writeCloseSpy{err: errors.New("switch-close")}
	cfgCloser := &closerSpy{}

	h := &Handler{
		Handler:          slog.NewJSONHandler(new(bytes.Buffer), nil),
		internalLogger:   slog.New(slog.DiscardHandler),
		switchableWriter: NewSwitchableWriter(switchWriter),
		ownedFile:        file,
		cfg:              &handlerConfig{ClosableWriter: cfgCloser},
	}

	if err := h.Close(); !errors.Is(err, switchWriter.err) {
		t.Fatalf("Handler.Close() error = %v, want %v", err, switchWriter.err)
	}
	if switchWriter.closed != 1 {
		t.Fatalf("switch writer close count = %d, want 1", switchWriter.closed)
	}
	if cfgCloser.closed != 1 {
		t.Fatalf("config closer closed = %d, want 1", cfgCloser.closed)
	}
	if _, err := file.WriteString("again"); err == nil {
		t.Fatalf("write to closed file unexpectedly succeeded")
	} else if !errors.Is(err, os.ErrClosed) && !errors.Is(err, os.ErrInvalid) {
		t.Fatalf("write error = %v, want os.ErrClosed or os.ErrInvalid", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("second Handler.Close() returned %v, want nil", err)
	}
}

// TestHandlerCloseReturnsAsyncHandlerError ensures Close reports errors from the async wrapper path.
func TestHandlerCloseReturnsAsyncHandlerError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("async-close")
	inner := closeErrorHandler{
		Handler: slog.NewJSONHandler(io.Discard, nil),
		err:     sentinel,
	}
	wrapped := slogcpasync.Wrap(inner, slogcpasync.WithEnabled(true))
	asyncHandler, ok := wrapped.(*slogcpasync.Handler)
	if !ok {
		t.Fatalf("slogcpasync.Wrap() returned %T, want *slogcpasync.Handler", wrapped)
	}

	h := &Handler{asyncHandler: asyncHandler}
	err := h.Close()
	if err == nil {
		t.Fatalf("Handler.Close() = nil, want error")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("Handler.Close() error = %v, want %v", err, sentinel)
	}
	if !strings.Contains(err.Error(), "close async handler") {
		t.Fatalf("Handler.Close() error = %q, want close async handler prefix", err.Error())
	}
	if err := h.Close(); err != nil {
		t.Fatalf("second Handler.Close() = %v, want nil", err)
	}
}

// TestHandlerCloseReportsOwnedFileErrors exercises the log file close error branch.
func TestHandlerCloseReportsOwnedFileErrors(t *testing.T) {
	t.Parallel()

	h := &Handler{
		internalLogger: slog.New(slog.DiscardHandler),
		ownedFile:      os.NewFile(^uintptr(0)>>1, "invalid"),
	}
	if err := h.Close(); err == nil {
		t.Fatalf("Handler.Close() = nil, want error from invalid file")
	}
}

// TestHandlerReopenLogFileWarnsOnCloseError ensures warnings do not abort reopen.
func TestHandlerReopenLogFileWarnsOnCloseError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	logPath := filepath.Join(dir, "app.log")

	h := &Handler{
		cfg: &handlerConfig{
			FilePath: logPath,
		},
		switchableWriter: NewSwitchableWriter(io.Discard),
		internalLogger:   slog.New(slog.DiscardHandler),
		ownedFile:        os.NewFile(^uintptr(0)>>1, "invalid"),
	}

	if err := h.ReopenLogFile(); err != nil {
		t.Fatalf("ReopenLogFile() = %v, want nil despite close warning", err)
	}
	if h.ownedFile == nil {
		t.Fatalf("ownedFile not updated after reopen")
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Handler.Close() after reopen returned %v", err)
	}
}

// TestHandlerCloseReturnsConfigCloserError ensures ClosableWriter errors surface when no other errors occur.
func TestHandlerCloseReturnsConfigCloserError(t *testing.T) {
	t.Parallel()

	cfgCloser := &closerSpy{err: errors.New("config-close")}
	h := &Handler{
		Handler:        slog.NewJSONHandler(new(bytes.Buffer), nil),
		internalLogger: slog.New(slog.DiscardHandler),
		cfg:            &handlerConfig{ClosableWriter: cfgCloser},
	}

	err := h.Close()
	if !errors.Is(err, cfgCloser.err) {
		t.Fatalf("Handler.Close() error = %v, want %v", err, cfgCloser.err)
	}
	if cfgCloser.closed != 1 {
		t.Fatalf("config closer closed = %d, want 1", cfgCloser.closed)
	}
}

// TestHandlerReopenLogFileSuccess ensures ReopenLogFile rotates descriptors and updates the switchable writer.
func TestHandlerReopenLogFileSuccess(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	logPath := filepath.Join(dir, "app.log")

	oldFile, err := os.CreateTemp(dir, "old-log-*.txt")
	if err != nil {
		t.Fatalf("os.CreateTemp() = %v", err)
	}

	h := &Handler{
		cfg:              &handlerConfig{FilePath: logPath},
		switchableWriter: NewSwitchableWriter(io.Discard),
		internalLogger:   slog.New(slog.DiscardHandler),
		ownedFile:        oldFile,
	}

	if err := h.ReopenLogFile(); err != nil {
		t.Fatalf("ReopenLogFile() returned %v, want nil", err)
	}

	// The previous file should be closed.
	if _, err := oldFile.WriteString("stale"); err == nil {
		t.Fatalf("expected write to closed file to fail")
	}

	currentWriter := h.switchableWriter.GetCurrentWriter()
	file, ok := currentWriter.(*os.File)
	if !ok {
		t.Fatalf("current writer type = %T, want *os.File", currentWriter)
	}
	if file.Name() != logPath {
		t.Fatalf("writer path = %q, want %q", file.Name(), logPath)
	}
	if _, err := file.WriteString("hello\n"); err != nil {
		t.Fatalf("write to reopened file returned %v", err)
	}

	if err := h.Close(); err != nil {
		t.Fatalf("Handler.Close() after reopen returned %v", err)
	}
}

type nilChildHandler struct{}

// Enabled implements slog.Handler.
func (nilChildHandler) Enabled(context.Context, slog.Level) bool { return true }

// Handle implements slog.Handler.
func (nilChildHandler) Handle(context.Context, slog.Record) error { return nil }

// WithAttrs implements slog.Handler and returns nil to simulate handler failure.
func (nilChildHandler) WithAttrs([]slog.Attr) slog.Handler { return nil }

// WithGroup implements slog.Handler and returns nil to simulate handler failure.
func (nilChildHandler) WithGroup(string) slog.Handler { return nil }

type writeCloseSpy struct {
	closed int
	err    error
}

// Write implements io.Writer.
func (w *writeCloseSpy) Write(p []byte) (int, error) { return len(p), nil }

// Close implements io.Closer and records invocations.
func (w *writeCloseSpy) Close() error {
	w.closed++
	return w.err
}

type closerSpy struct {
	closed int
	err    error
}

// Close implements io.Closer and records invocations.
func (c *closerSpy) Close() error {
	c.closed++
	return c.err
}

type closeErrorHandler struct {
	slog.Handler
	err error
}

// Close implements io.Closer and returns the configured error.
func (h closeErrorHandler) Close() error {
	return h.err
}

// TestHandlerReopenLogFileNoopWithoutFile ensures file rotation is a no-op when no file is configured.
func TestHandlerReopenLogFileNoopWithoutFile(t *testing.T) {
	t.Parallel()

	var h Handler
	if err := h.ReopenLogFile(); err != nil {
		t.Fatalf("ReopenLogFile() = %v, want nil", err)
	}
}

// TestHandlerReopenLogFileReportsErrors exercises the error branch used when reopen fails.
func TestHandlerReopenLogFileReportsErrors(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	missingPath := filepath.Join(dir, "missing", "app.log")

	h := &Handler{
		cfg:              &handlerConfig{FilePath: missingPath},
		switchableWriter: NewSwitchableWriter(io.Discard),
		internalLogger:   slog.New(slog.DiscardHandler),
	}

	err := h.ReopenLogFile()
	if err == nil {
		t.Fatalf("ReopenLogFile() = nil, want error")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReopenLogFile() error = %v, want wrapping os.ErrNotExist", err)
	}
}

// TestHandlerSetupHelpers exercises setup helpers and pipeline construction paths.
func TestHandlerSetupHelpers(t *testing.T) {
	builder := collectOptions([]Option{WithLevel(slog.LevelWarn), nil})
	if builder.level == nil || *builder.level != slog.LevelWarn {
		t.Fatalf("collectOptions level = %v, want LevelWarn", builder.level)
	}

	custom := slog.New(slog.DiscardHandler)
	if got := ensureInternalLogger(custom); got != custom {
		t.Fatalf("ensureInternalLogger returned %v, want provided logger", got)
	}
	if got := ensureInternalLogger(nil); got == nil {
		t.Fatalf("ensureInternalLogger(nil) returned nil")
	}

	cfg := handlerConfig{
		TraceDiagnostics: TraceDiagnosticsWarnOnce,
		TraceProjectID:   "runtime-project",
	}
	if err := prepareRuntimeConfig(&cfg); err != nil {
		t.Fatalf("prepareRuntimeConfig returned %v", err)
	}
	if cfg.TraceProjectID != "runtime-project" {
		t.Fatalf("TraceProjectID = %q, want runtime-project", cfg.TraceProjectID)
	}
	if cfg.traceDiagnosticsState == nil {
		t.Fatalf("traceDiagnosticsState not initialized")
	}

	strictCfg := handlerConfig{TraceDiagnostics: TraceDiagnosticsStrict}
	if err := prepareRuntimeConfig(&strictCfg); err == nil {
		t.Fatalf("prepareRuntimeConfig strict mode without project should error")
	}

	lv := resolveLevelVar(nil, slog.LevelDebug)
	if lv == nil || lv.Level() != slog.LevelDebug {
		t.Fatalf("resolveLevelVar created %v, want LevelDebug", lv)
	}
	resolveLevelVar(lv, slog.LevelError)
	if lv.Level() != slog.LevelError {
		t.Fatalf("resolveLevelVar should mutate existing LevelVar to LevelError, got %v", lv.Level())
	}

	writerCfg := handlerConfig{}
	ensureWriterDefaults(&writerCfg, nil)
	if writerCfg.Writer == nil || !writerCfg.writerExternallyOwned {
		t.Fatalf("ensureWriterDefaults should set stdout with external ownership, got %#v", writerCfg)
	}
	writerCfg.Writer = nil
	ensureWriterFallback(&writerCfg)
	if writerCfg.Writer == nil {
		t.Fatalf("ensureWriterFallback should set a writer when nil")
	}

	mwCalled := atomic.Bool{}
	mw := func(next slog.Handler) slog.Handler {
		return &middlewareRecorder{next: next, called: &mwCalled}
	}

	pipelineCfg := &handlerConfig{
		Level:       slog.LevelInfo,
		Writer:      io.Discard,
		Middlewares: []Middleware{mw},
		AddSource:   true,
	}
	pipelineOpts := &options{asyncEnabled: false}
	levelVar := new(slog.LevelVar)
	handler, _ := buildPipeline(pipelineCfg, levelVar, slog.New(slog.DiscardHandler), pipelineOpts)

	if _, ok := handler.(sourceAwareHandler); !ok {
		t.Fatalf("buildPipeline should wrap handler with source awareness")
	}
	logger := slog.New(handler)
	logger.Info("pipeline-check")
	if !mwCalled.Load() {
		t.Fatalf("middleware was not invoked by pipeline")
	}

	if closer, ok := handler.(interface{ Close() error }); ok {
		_ = closer.Close()
	}
}

// TestHandlerLifecycleAndLoggingWrappers covers Close/Reopen and wrapper helpers.
func TestHandlerLifecycleAndLoggingWrappers(t *testing.T) {
	if h := (*Handler)(nil); h.Level() != slog.LevelInfo {
		t.Fatalf("nil handler Level should default to LevelInfo")
	}
	var nilHandler *Handler
	nilHandler.SetLevel(slog.LevelDebug)
	if nilHandler.LevelVar() != nil {
		t.Fatalf("LevelVar on nil handler should be nil")
	}

	emptyCfg := handlerConfig{}
	if file, switcher, err := prepareFileWriter(&emptyCfg); file != nil || switcher != nil || err != nil {
		t.Fatalf("prepareFileWriter with empty path = (%v,%v,%v), want nils", file, switcher, err)
	}

	skipCloseCfg := handlerConfig{Writer: io.Discard, writerExternallyOwned: true}
	ensureClosableWriter(&skipCloseCfg)
	if skipCloseCfg.ClosableWriter != nil {
		t.Fatalf("ensureClosableWriter should not set closer when writer is externally owned")
	}

	closableCfg := handlerConfig{Writer: &trackingCloser{}}
	ensureClosableWriter(&closableCfg)
	if closableCfg.ClosableWriter == nil {
		t.Fatalf("ensureClosableWriter should capture io.Closer writers")
	}

	path := filepath.Join(t.TempDir(), "handler.log")
	cfg := handlerConfig{FilePath: path}

	ownedFile, switchWriter, err := prepareFileWriter(&cfg)
	if err != nil {
		t.Fatalf("prepareFileWriter returned %v", err)
	}
	if switchWriter == nil || cfg.Writer == nil {
		t.Fatalf("expected switchable writer to be configured")
	}
	if err := os.WriteFile(path, []byte("seed\n"), 0o600); err != nil {
		t.Fatalf("seed write failed: %v", err)
	}

	closerWriter := &trackingCloser{}
	cfg.ClosableWriter = closerWriter
	handler := &Handler{
		cfg:              &cfg,
		internalLogger:   slog.New(slog.DiscardHandler),
		switchableWriter: switchWriter,
		ownedFile:        ownedFile,
		levelVar:         new(slog.LevelVar),
	}

	if err := handler.closeResources(); err != nil {
		t.Fatalf("closeResources returned %v", err)
	}
	if !closerWriter.closed.Load() {
		t.Fatalf("closeResources should close configured writer")
	}

	cfg2 := handlerConfig{FilePath: path}
	file2, switchWriter2, err := prepareFileWriter(&cfg2)
	if err != nil {
		t.Fatalf("second prepareFileWriter returned %v", err)
	}
	handler2 := &Handler{
		cfg:              &cfg2,
		internalLogger:   slog.New(slog.DiscardHandler),
		switchableWriter: switchWriter2,
		ownedFile:        file2,
		levelVar:         new(slog.LevelVar),
	}
	if err := handler2.ReopenLogFile(); err != nil {
		t.Fatalf("ReopenLogFile returned %v", err)
	}
	_ = handler2.Close()

	handler.levelVar = new(slog.LevelVar)
	handler.SetLevel(slog.LevelWarn)
	if got := handler.Level(); got != slog.LevelWarn {
		t.Fatalf("Level = %v, want LevelWarn", got)
	}
	if handler.LevelVar() == nil {
		t.Fatalf("LevelVar should return non-nil when configured")
	}

	recorder := &captureHandler{}
	logger := slog.New(recorder)
	Default(logger, "default")
	DefaultContext(context.Background(), logger, "default-context")
	Debug(logger, "debug")
	DebugContext(context.Background(), logger, "debug-context")
	Info(logger, "info")
	InfoContext(context.Background(), logger, "info-context")
	Warn(logger, "warn")
	WarnContext(context.Background(), logger, "warn-context")
	Error(logger, "error")
	ErrorContext(context.Background(), logger, "error-context")
	NoticeContext(context.Background(), logger, "notice")
	CriticalContext(context.Background(), logger, "critical")
	AlertContext(context.Background(), logger, "alert")
	EmergencyContext(context.Background(), logger, "emergency")

	if recorder.count == 0 {
		t.Fatalf("wrapper helpers did not emit any records")
	}
}

type middlewareRecorder struct {
	next   slog.Handler
	called *atomic.Bool
}

// Enabled delegates to the wrapped handler.
func (m *middlewareRecorder) Enabled(ctx context.Context, lvl slog.Level) bool {
	return m.next.Enabled(ctx, lvl)
}

// Handle records invocation and forwards to the wrapped handler.
func (m *middlewareRecorder) Handle(ctx context.Context, r slog.Record) error {
	m.called.Store(true)
	return m.next.Handle(ctx, r)
}

// WithAttrs propagates attribute state to the wrapped handler.
func (m *middlewareRecorder) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &middlewareRecorder{next: m.next.WithAttrs(attrs), called: m.called}
}

// WithGroup propagates grouping to the wrapped handler.
func (m *middlewareRecorder) WithGroup(name string) slog.Handler {
	return &middlewareRecorder{next: m.next.WithGroup(name), called: m.called}
}

// captureHandler records incoming log records for wrapper helper assertions.
type captureHandler struct {
	last  slog.Record
	count int
}

// Enabled always returns true for captureHandler.
func (c *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

// Handle stores the record for later inspection.
func (c *captureHandler) Handle(_ context.Context, r slog.Record) error {
	c.last = r.Clone()
	c.count++
	return nil
}

// WithAttrs returns itself for captureHandler to satisfy slog.Handler.
func (c *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return c }

// WithGroup returns itself for captureHandler to satisfy slog.Handler.
func (c *captureHandler) WithGroup(string) slog.Handler { return c }

type trackingCloser struct {
	closed atomic.Bool
}

// Write discards data to satisfy io.Writer.
func (t *trackingCloser) Write(p []byte) (int, error) { return len(p), nil }

// Close marks the closer as closed.
func (t *trackingCloser) Close() error {
	t.closed.Store(true)
	return nil
}

// TestContextHelpersNilLogger ensures the contextual logging helpers safely
// handle a nil slog.Logger without panicking.
func TestContextHelpersNilLogger(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	helpers := map[string]func(context.Context, *slog.Logger, string, ...any){
		"DefaultContext":   DefaultContext,
		"DebugContext":     DebugContext,
		"InfoContext":      InfoContext,
		"WarnContext":      WarnContext,
		"ErrorContext":     ErrorContext,
		"NoticeContext":    NoticeContext,
		"CriticalContext":  CriticalContext,
		"AlertContext":     AlertContext,
		"EmergencyContext": EmergencyContext,
	}

	for name, fn := range helpers {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fn(ctx, nil, "msg")
		})
	}
}
