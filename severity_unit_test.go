//go:build unit
// +build unit

package slogcp_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/pjscruggs/slogcp"
)

func TestSeverityMappingMatchesNativeAndGCPLevels(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	h, err := slogcp.NewHandler(io.Discard, slogcp.WithRedirectWriter(&buf), slogcp.WithLevel(slog.LevelDebug))
	if err != nil {
		t.Fatalf("NewHandler() returned %v, want nil", err)
	}
	t.Cleanup(func() {
		if cerr := h.Close(); cerr != nil {
			t.Errorf("Handler.Close() returned %v, want nil", cerr)
		}
	})

	logger := slog.New(h)
	ctx := context.Background()

	testCases := []struct {
		log          func()
		wantMsg      string
		wantSeverity string
	}{
		{log: func() { logger.Debug("native-debug") }, wantMsg: "native-debug", wantSeverity: "DEBUG"},
		{log: func() { logger.Info("native-info") }, wantMsg: "native-info", wantSeverity: "INFO"},
		{log: func() { logger.Warn("native-warn") }, wantMsg: "native-warn", wantSeverity: "WARNING"},
		{log: func() { logger.Error("native-error") }, wantMsg: "native-error", wantSeverity: "ERROR"},
		{log: func() { logger.Log(ctx, slogcp.LevelNotice.Level(), "gcp-notice") }, wantMsg: "gcp-notice", wantSeverity: "NOTICE"},
		{log: func() { logger.Log(ctx, slogcp.LevelCritical.Level(), "gcp-critical") }, wantMsg: "gcp-critical", wantSeverity: "CRITICAL"},
		{log: func() { logger.Log(ctx, slogcp.LevelAlert.Level(), "gcp-alert") }, wantMsg: "gcp-alert", wantSeverity: "ALERT"},
		{log: func() { logger.Log(ctx, slogcp.LevelEmergency.Level(), "gcp-emergency") }, wantMsg: "gcp-emergency", wantSeverity: "EMERGENCY"},
		{log: func() { logger.Log(ctx, slogcp.LevelDefault.Level(), "gcp-default") }, wantMsg: "gcp-default", wantSeverity: "DEFAULT"},
	}

	for _, tc := range testCases {
		tc.log()
	}

	entries := decodeLogBuffer(t, &buf)
	if len(entries) != len(testCases) {
		t.Fatalf("decodeLogBuffer() returned %d entries, want %d", len(entries), len(testCases))
	}

	for i, tc := range testCases {
		entry := entries[i]
		if got := entry["message"]; got != tc.wantMsg {
			t.Fatalf("entry %d message = %v, want %q", i, got, tc.wantMsg)
		}
		gotSeverity, ok := entry["severity"].(string)
		if !ok {
			t.Fatalf("entry %d severity type = %T, want string", i, entry["severity"])
		}
		if gotSeverity != tc.wantSeverity {
			t.Fatalf("entry %d severity = %q, want %q", i, gotSeverity, tc.wantSeverity)
		}
	}
}

func TestDefaultSeverityHelpersEmitDefault(t *testing.T) {
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
	ctx := context.Background()

	slogcp.Default(logger, "default-no-context")
	slogcp.DefaultContext(ctx, logger, "default-with-context")

	entries := decodeLogBuffer(t, &buf)
	if len(entries) != 2 {
		t.Fatalf("decodeLogBuffer() returned %d entries, want 2", len(entries))
	}

	expected := map[string]string{
		"default-no-context":   "DEFAULT",
		"default-with-context": "DEFAULT",
	}

	for _, entry := range entries {
		msg, ok := entry["message"].(string)
		if !ok {
			t.Fatalf("entry message type = %T, want string", entry["message"])
		}
		wantSeverity, ok := expected[msg]
		if !ok {
			t.Fatalf("unexpected message %q in log entries", msg)
		}
		gotSeverity, ok := entry["severity"].(string)
		if !ok {
			t.Fatalf("entry severity type = %T, want string", entry["severity"])
		}
		if gotSeverity != wantSeverity {
			t.Fatalf("message %q severity = %q, want %q", msg, gotSeverity, wantSeverity)
		}
		delete(expected, msg)
	}

	if len(expected) != 0 {
		t.Fatalf("expected messages missing from log output: %v", expected)
	}
}

func TestDefaultSeverityNotFilteredByHigherMinimum(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	h, err := slogcp.NewHandler(io.Discard, slogcp.WithRedirectWriter(&buf), slogcp.WithLevel(slog.LevelError))
	if err != nil {
		t.Fatalf("NewHandler() returned %v, want nil", err)
	}
	t.Cleanup(func() {
		if cerr := h.Close(); cerr != nil {
			t.Errorf("Handler.Close() returned %v, want nil", cerr)
		}
	})

	logger := slog.New(h)
	ctx := context.Background()

	logger.Info("suppressed-info")
	slogcp.Default(logger, "default-no-context")
	slogcp.DefaultContext(ctx, logger, "default-with-context")

	entries := decodeLogBuffer(t, &buf)
	if len(entries) != 2 {
		t.Fatalf("decodeLogBuffer() returned %d entries, want 2", len(entries))
	}

	for i, entry := range entries {
		gotSeverity, ok := entry["severity"].(string)
		if !ok {
			t.Fatalf("entry %d severity type = %T, want string", i, entry["severity"])
		}
		if gotSeverity != "DEFAULT" {
			t.Fatalf("entry %d severity = %q, want DEFAULT", i, gotSeverity)
		}
		if entry["message"] == "suppressed-info" {
			t.Fatalf("info-level log unexpectedly emitted at index %d", i)
		}
	}
}
