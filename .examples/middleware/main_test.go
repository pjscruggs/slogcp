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

package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/pjscruggs/slogcp"
)

// TestMiddlewareRedactsSecretsAndCapturesStacks ensures the middleware example
// masks sensitive attributes while enabling stack traces for warnings.
func TestMiddlewareRedactsSecretsAndCapturesStacks(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	handler, err := slogcp.NewHandler(&buf,
		slogcp.WithStackTraceEnabled(true),
		slogcp.WithStackTraceLevel(slog.LevelWarn),
		slogcp.WithMiddleware(redactSecretsMiddleware),
	)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	t.Cleanup(func() {
		if cerr := handler.Close(); cerr != nil {
			t.Errorf("handler close: %v", cerr)
		}
	})

	logger := slog.New(handler)
	logger.Info("issuing request",
		slog.String("api_key", "secret-123"),
		slog.String("user", "tulip"),
	)
	logger.Warn("slow response encountered", slog.String("trace_id", "abc123"))

	entries := decodeEntries(t, &buf)
	if len(entries) != 2 {
		t.Fatalf("expected 2 log entries, got %d", len(entries))
	}

	if got := entries[0]["api_key"]; got != "[redacted]" {
		t.Fatalf("api_key = %v, want [redacted]", got)
	}
	stack, _ := entries[1]["stack_trace"].(string)
	if stack == "" {
		t.Fatalf("warn entry missing stack_trace: %#v", entries[1])
	}
}

// decodeEntries unmarshals newline separated JSON log entries from buf.
func decodeEntries(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()

	data := bytes.TrimSpace(buf.Bytes())
	if len(data) == 0 {
		t.Fatalf("expected log output")
	}

	lines := bytes.Split(data, []byte("\n"))
	entries := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("unmarshal log entry: %v", err)
		}
		entries = append(entries, entry)
	}
	return entries
}
