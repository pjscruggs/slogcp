//go:build unit
// +build unit

package slogcp_test

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/pjscruggs/slogcp"
)

// TestCaptureStackProducesGoFormat ensures CaptureStack emits Go runtime-style stacks and frame metadata.
func TestCaptureStackProducesGoFormat(t *testing.T) {
	t.Parallel()

	stack, frame := slogcp.CaptureStack(nil)
	if stack == "" {
		t.Fatal("CaptureStack returned an empty stack trace")
	}
	if frame.Function == "" {
		t.Fatal("CaptureStack returned an empty frame function name")
	}

	lines := strings.Split(stack, "\n")
	if len(lines) < 3 {
		t.Fatalf("stack trace has insufficient lines: %q", stack)
	}

	header := lines[0]
	if !strings.HasPrefix(header, "goroutine ") || !strings.HasSuffix(header, "]:") {
		t.Fatalf("stack trace header %q is not in Go runtime format", header)
	}

	firstFunc := lines[1]
	if !strings.Contains(firstFunc, "TestCaptureStackProducesGoFormat") {
		t.Fatalf("expected test function name in stack trace, got %q", firstFunc)
	}

	firstLoc := lines[2]
	if !strings.HasPrefix(firstLoc, "\t") {
		t.Fatalf("expected location line to start with a tab, got %q", firstLoc)
	}
	if !strings.Contains(firstLoc, ":") {
		t.Fatalf("expected location line to contain file:line information, got %q", firstLoc)
	}

	if !strings.Contains(stack, frame.Function) {
		t.Fatalf("stack trace does not contain returned frame function %q", frame.Function)
	}
}

// TestHandlerEmitsStackTraceForErrors verifies handlers include stack traces when enabled.
func TestHandlerEmitsStackTraceForErrors(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	handler, err := slogcp.NewHandler(&buf, slogcp.WithStackTraceEnabled(true))
	if err != nil {
		t.Fatalf("NewHandler() returned %v", err)
	}
	t.Cleanup(func() {
		if cerr := handler.Close(); cerr != nil {
			t.Errorf("Handler.Close() returned %v", cerr)
		}
	})

	logger := slog.New(handler)
	testErr := errors.New("boom")

	logger.ErrorContext(context.Background(), "failed to do thing", slog.Any("error", testErr))

	content := strings.TrimSpace(buf.String())
	if content == "" {
		t.Fatal("log output empty")
	}

	var entry map[string]any
	if err := json.Unmarshal([]byte(content), &entry); err != nil {
		t.Fatalf("json.Unmarshal() returned %v", err)
	}

	sev, ok := entry["severity"].(string)
	if !ok || sev != "E" {
		t.Fatalf("expected severity E, got %v", entry["severity"])
	}

	stackVal, ok := entry["stack_trace"].(string)
	if !ok || stackVal == "" {
		t.Fatalf("expected stack_trace string in log entry, got %v", entry["stack_trace"])
	}

	if !strings.HasPrefix(stackVal, "goroutine ") {
		t.Fatalf("stack_trace does not include goroutine header: %q", stackVal)
	}
	if !strings.Contains(stackVal, "\n\t") {
		t.Fatalf("stack_trace does not contain Go frame separators: %q", stackVal)
	}
}
