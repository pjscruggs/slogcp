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
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pjscruggs/slogcp"
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
	h, err := slogcp.NewHandler(io.Discard, slogcp.WithRedirectWriter(&buf))
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
	h, err := slogcp.NewHandler(io.Discard, slogcp.WithRedirectWriter(cw))
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
	h, err := slogcp.NewHandler(io.Discard,
		slogcp.WithRedirectWriter(&buf),
		slogcp.WithReplaceAttr(func(groups []string, attr slog.Attr) slog.Attr {
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

	h, err := slogcp.NewHandler(io.Discard, slogcp.WithRedirectToFile(logPath))
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
	h, err := slogcp.NewHandler(io.Discard, slogcp.WithRedirectWriter(&buf))
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
	h, err := slogcp.NewHandler(io.Discard, slogcp.WithRedirectWriter(&buf))
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
	h, err := slogcp.NewHandler(io.Discard, slogcp.WithRedirectWriter(&buf), slogcp.WithLevel(slog.LevelInfo))
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
	levelVar.Set(slog.LevelWarn)

	h, err := slogcp.NewHandler(io.Discard, slogcp.WithRedirectWriter(&buf), slogcp.WithLevelVar(levelVar))
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

// TestHandlerTimeFieldEmission verifies the handler omits the "time" field by
// default but can emit it when explicitly enabled.
func TestHandlerTimeFieldEmission(t *testing.T) {
	t.Parallel()

	t.Run("disabled_by_default", func(t *testing.T) {
		t.Parallel()

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
		h, err := slogcp.NewHandler(io.Discard,
			slogcp.WithRedirectWriter(&buf),
			slogcp.WithTime(true),
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

	h, err := slogcp.NewHandler(
		io.Discard,
		slogcp.WithRedirectWriter(&buf),
		slogcp.WithLevelVar(levelVar),
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

	h, err := slogcp.NewHandler(
		io.Discard,
		slogcp.WithRedirectWriter(&sink),
		slogcp.WithInternalLogger(internalLogger),
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
	fail := &failingWriter{err: errors.New("sink down")}

	h, err := slogcp.NewHandler(
		io.Discard,
		slogcp.WithRedirectWriter(fail),
		slogcp.WithInternalLogger(internalLogger),
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

type failingWriter struct {
	err error
}

// Write always returns an error to simulate a broken sink.
func (f *failingWriter) Write([]byte) (int, error) {
	return 0, f.err
}

// TestSeverityHelpersEmitExpectedLevels exercises the severity-specific helpers to
// ensure they log at the intended slogcp levels and tolerate nil loggers.
func TestSeverityHelpersEmitExpectedLevels(t *testing.T) {
	t.Parallel()

	recorder := &severityRecordingHandler{}
	logger := slog.New(recorder)
	ctx := context.Background()

	slogcp.DefaultContext(ctx, logger, "default")
	slogcp.NoticeContext(ctx, logger, "notice")
	slogcp.CriticalContext(ctx, logger, "critical")
	slogcp.AlertContext(ctx, logger, "alert")
	slogcp.EmergencyContext(ctx, logger, "emergency")

	// Ensure helpers do nothing when the logger is nil.
	slogcp.AlertContext(ctx, nil, "ignored")

	want := []struct {
		msg   string
		level slog.Level
	}{
		{"default", slog.Level(slogcp.LevelDefault)},
		{"notice", slog.Level(slogcp.LevelNotice)},
		{"critical", slog.Level(slogcp.LevelCritical)},
		{"alert", slog.Level(slogcp.LevelAlert)},
		{"emergency", slog.Level(slogcp.LevelEmergency)},
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

// TestHandlerStackTraceLevelEmitsStacks verifies the stack trace level option triggers capture.
func TestHandlerStackTraceLevelEmitsStacks(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	h, err := slogcp.NewHandler(&buf,
		slogcp.WithStackTraceEnabled(true),
		slogcp.WithStackTraceLevel(slog.LevelWarn),
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

// TestHandlerMiddlewareInvokesHooks ensures slogcp.WithMiddleware attaches middleware correctly.
func TestHandlerMiddlewareInvokesHooks(t *testing.T) {
	t.Parallel()

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

	h, err := slogcp.NewHandler(&buf, slogcp.WithMiddleware(middleware))
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
			reader.Close()
		}()

		h, err := slogcp.NewHandler(io.Discard, slogcp.WithRedirectToStdout(), slogcp.WithSeverityAliases(false))
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
			reader.Close()
		}()

		h, err := slogcp.NewHandler(io.Discard, slogcp.WithRedirectToStderr(), slogcp.WithSeverityAliases(false))
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
			writer.Close()
			os.Stdout = prev
		}
	case "stderr":
		prev := os.Stderr
		os.Stderr = writer
		return reader, func() {
			writer.Close()
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

	var h *slogcp.Handler
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
			call: func(l *slog.Logger) { slogcp.Default(l, "default event") },
			want: slog.Level(slogcp.LevelDefault),
		},
		{
			name: "default_context",
			call: func(l *slog.Logger) { slogcp.DefaultContext(context.Background(), l, "ctx default") },
			want: slog.Level(slogcp.LevelDefault),
		},
		{
			name: "notice",
			call: func(l *slog.Logger) { slogcp.NoticeContext(context.Background(), l, "notice") },
			want: slog.Level(slogcp.LevelNotice),
		},
		{
			name: "critical",
			call: func(l *slog.Logger) { slogcp.CriticalContext(context.Background(), l, "crit") },
			want: slog.Level(slogcp.LevelCritical),
		},
		{
			name: "alert",
			call: func(l *slog.Logger) { slogcp.AlertContext(context.Background(), l, "alert") },
			want: slog.Level(slogcp.LevelAlert),
		},
		{
			name: "emergency",
			call: func(l *slog.Logger) { slogcp.EmergencyContext(context.Background(), l, "emerg") },
			want: slog.Level(slogcp.LevelEmergency),
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			recorder := &recordingHandler{}
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
	slogcp.Default(nil, "noop")
	slogcp.DefaultContext(ctx, nil, "noop")
	slogcp.NoticeContext(ctx, nil, "noop")
	slogcp.CriticalContext(ctx, nil, "noop")
	slogcp.AlertContext(ctx, nil, "noop")
	slogcp.EmergencyContext(ctx, nil, "noop")
}
