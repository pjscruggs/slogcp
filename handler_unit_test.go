//go:build unit
// +build unit

package slogcp_test

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pjscruggs/slogcp"
)

type closingBuffer struct {
	bytes.Buffer
	closed bool
}

func (c *closingBuffer) Close() error {
	c.closed = true
	return nil
}

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

func TestHandlerCloseClosesRedirectWriter(t *testing.T) {
	t.Parallel()

	cw := &closingBuffer{}
	h, err := slogcp.NewHandler(io.Discard, slogcp.WithRedirectWriter(cw))
	if err != nil {
		t.Fatalf("NewHandler() returned %v, want nil", err)
	}

	if err := h.Close(); err != nil {
		t.Fatalf("Handler.Close() returned %v, want nil", err)
	}
	if !cw.closed {
		t.Fatalf("redirect writer was not closed")
	}
}

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
