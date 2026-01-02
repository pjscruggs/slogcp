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

package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/pjscruggs/slogcp"
)

// TestDynamicLevelAdjustments verifies that runtime level configuration
// suppresses and re-enables emissions as demonstrated in the example.
func TestDynamicLevelAdjustments(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	h, err := slogcp.NewHandler(&buf)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	t.Cleanup(func() {
		if cerr := h.Close(); cerr != nil {
			t.Errorf("handler close: %v", cerr)
		}
	})

	logger := slog.New(h)

	h.SetLevel(slog.LevelWarn)
	logger.Debug("suppressed debug")
	logger.Warn("raising minimum level to warn")

	h.LevelVar().Set(slog.LevelDebug)
	logger.Debug("debug logging re-enabled via shared LevelVar")

	entries := decodeEntries(t, &buf)
	if len(entries) != 2 {
		t.Fatalf("expected 2 log entries, got %d", len(entries))
	}

	if msg := entries[0]["message"]; msg != "raising minimum level to warn" {
		t.Fatalf("first message = %v, want %q", msg, "raising minimum level to warn")
	}
	if msg := entries[1]["message"]; msg != "debug logging re-enabled via shared LevelVar" {
		t.Fatalf("second message = %v, want %q", msg, "debug logging re-enabled via shared LevelVar")
	}
}

// decodeEntries parses all JSON log lines from the buffer.
func decodeEntries(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()

	content := bytes.TrimSpace(buf.Bytes())
	if len(content) == 0 {
		t.Fatalf("expected log output")
	}

	lines := bytes.Split(content, []byte("\n"))
	entries := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("unmarshal log line: %v", err)
		}
		entries = append(entries, entry)
	}
	return entries
}
