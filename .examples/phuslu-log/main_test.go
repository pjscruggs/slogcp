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
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pjscruggs/slogcp"
)

// TestAsyncFileLoggerWritesEntries validates that the phuslu-based async
// writer flushes slogcp output to disk and keeps structured fields intact.
func TestAsyncFileLoggerWritesEntries(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "service.log")

	asyncWriter := newAsyncFileWriter(logPath)
	handler, err := slogcp.NewHandler(os.Stdout, slogcp.WithRedirectWriter(asyncWriter))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	logger := slog.New(handler)

	const wantMessage = "async log persisted"
	logger.Info(wantMessage, slog.String("component", "test-harness"))

	if err := handler.Close(); err != nil {
		t.Fatalf("handler.Close: %v", err)
	}
	if err := asyncWriter.Close(); err != nil {
		t.Fatalf("asyncWriter.Close: %v", err)
	}

	logFile := latestLogFile(t, tmp, "service")
	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 0 {
		t.Fatalf("expected at least one log line in %s", logFile)
	}

	entry := decodeJSON(t, lines[len(lines)-1])
	if got := entry["message"]; got != wantMessage {
		t.Fatalf("message = %v, want %q", got, wantMessage)
	}
	if got := entry["component"]; got != "test-harness" {
		t.Fatalf("component = %v, want %q", got, "test-harness")
	}
}

func latestLogFile(t *testing.T, dir, prefix string) string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir %s: %v", dir, err)
	}

	var (
		latestPath string
		latestTime time.Time
	)

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".log") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if latestPath == "" || info.ModTime().After(latestTime) {
			latestPath = filepath.Join(dir, name)
			latestTime = info.ModTime()
		}
	}

	if latestPath == "" {
		t.Fatalf("no log files with prefix %q in %s", prefix, dir)
	}
	return latestPath
}

func decodeJSON(t *testing.T, line string) map[string]any {
	t.Helper()

	var entry map[string]any
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		t.Fatalf("unmarshal log entry: %v\nline: %s", err, line)
	}
	return entry
}
