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

//go:build unit
// +build unit

package slogcp_test

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"math"
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
	record.AddAttrs(slog.Float64("nan", math.NaN()))

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

func (f *failingWriter) Write([]byte) (int, error) {
	return 0, f.err
}
