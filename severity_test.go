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

package slogcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/pjscruggs/slogcp"
)

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

// TestSeverityMappingMatchesNativeAndGCPLevels ensures all native slog levels and
// slogcp's extended GCP levels serialize with the expected severity string.
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
		log       func()
		wantMsg   string
		wantAlias string
		wantFull  string
	}{
		{log: func() { logger.Debug("native-debug") }, wantMsg: "native-debug", wantAlias: "D", wantFull: "DEBUG"},
		{log: func() { logger.Info("native-info") }, wantMsg: "native-info", wantAlias: "I", wantFull: "INFO"},
		{log: func() { logger.Warn("native-warn") }, wantMsg: "native-warn", wantAlias: "W", wantFull: "WARNING"},
		{log: func() { logger.Error("native-error") }, wantMsg: "native-error", wantAlias: "E", wantFull: "ERROR"},
		{log: func() { logger.Log(ctx, slogcp.LevelNotice.Level(), "gcp-notice") }, wantMsg: "gcp-notice", wantAlias: "N", wantFull: "NOTICE"},
		{log: func() { logger.Log(ctx, slogcp.LevelCritical.Level(), "gcp-critical") }, wantMsg: "gcp-critical", wantAlias: "C", wantFull: "CRITICAL"},
		{log: func() { logger.Log(ctx, slogcp.LevelAlert.Level(), "gcp-alert") }, wantMsg: "gcp-alert", wantAlias: "A", wantFull: "ALERT"},
		{log: func() { logger.Log(ctx, slogcp.LevelEmergency.Level(), "gcp-emergency") }, wantMsg: "gcp-emergency", wantAlias: "EMERG", wantFull: "EMERGENCY"},
		{log: func() { logger.Log(ctx, slogcp.LevelDefault.Level(), "gcp-default") }, wantMsg: "gcp-default", wantAlias: "DEFAULT", wantFull: "DEFAULT"},
	}

	for _, tc := range testCases {
		tc.log()
	}

	entries := decodeLogBuffer(t, &buf)
	if len(entries) != len(testCases) {
		t.Fatalf("decodeLogBuffer() returned %d entries, want %d", len(entries), len(testCases))
	}

	useAliases := prefersManagedDefaults()

	for i, tc := range testCases {
		entry := entries[i]
		if got := entry["message"]; got != tc.wantMsg {
			t.Fatalf("entry %d message = %v, want %q", i, got, tc.wantMsg)
		}
		gotSeverity, ok := entry["severity"].(string)
		if !ok {
			t.Fatalf("entry %d severity type = %T, want string", i, entry["severity"])
		}
		want := tc.wantFull
		if useAliases {
			want = tc.wantAlias
		}
		if gotSeverity != want {
			t.Fatalf("entry %d severity = %q, want %q", i, gotSeverity, want)
		}
	}
}

// TestDefaultSeverityHelpersEmitDefault checks the helper convenience methods
// always emit logs with the DEFAULT severity label.
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

// TestDefaultSeverityNotFilteredByHigherMinimum verifies DEFAULT logs bypass
// handler minimum level filtering so operational events are preserved.
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

// TestHelperSeverityParity ensures slogcp.Debug/Info/Warn/Error (and their
// Context counterparts) emit the same severity strings as their native slog
// equivalents, preserving syntax parity for developers.
func TestHelperSeverityParity(t *testing.T) {
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

	slogcp.Debug(logger, "helper-debug")
	slogcp.Info(logger, "helper-info")
	slogcp.Warn(logger, "helper-warn")
	slogcp.Error(logger, "helper-error")
	slogcp.DebugContext(ctx, logger, "helper-debug-ctx")
	slogcp.InfoContext(ctx, logger, "helper-info-ctx")
	slogcp.WarnContext(ctx, logger, "helper-warn-ctx")
	slogcp.ErrorContext(ctx, logger, "helper-error-ctx")

	entries := decodeLogBuffer(t, &buf)
	if len(entries) != 8 {
		t.Fatalf("decodeLogBuffer() returned %d entries, want 8", len(entries))
	}

	useAliases := prefersManagedDefaults()
	expectations := []struct {
		msg   string
		full  string
		alias string
	}{
		{msg: "helper-debug", full: "DEBUG", alias: "D"},
		{msg: "helper-info", full: "INFO", alias: "I"},
		{msg: "helper-warn", full: "WARNING", alias: "W"},
		{msg: "helper-error", full: "ERROR", alias: "E"},
		{msg: "helper-debug-ctx", full: "DEBUG", alias: "D"},
		{msg: "helper-info-ctx", full: "INFO", alias: "I"},
		{msg: "helper-warn-ctx", full: "WARNING", alias: "W"},
		{msg: "helper-error-ctx", full: "ERROR", alias: "E"},
	}

	for i, expect := range expectations {
		entry := entries[i]
		msg, ok := entry["message"].(string)
		if !ok {
			t.Fatalf("entry %d message type = %T, want string", i, entry["message"])
		}
		if msg != expect.msg {
			t.Fatalf("entry %d message = %q, want %q", i, msg, expect.msg)
		}
		sev, ok := entry["severity"].(string)
		if !ok {
			t.Fatalf("entry %d severity type = %T, want string", i, entry["severity"])
		}
		want := expect.full
		if useAliases {
			want = expect.alias
		}
		if sev != want {
			t.Fatalf("entry %d severity = %q, want %q", i, sev, want)
		}
	}
}
