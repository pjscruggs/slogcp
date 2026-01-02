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
	"compress/gzip"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/DeRuina/timberjack"

	"github.com/pjscruggs/slogcp"
)

// TestTimberjackIntegration exercises the timberjack example by writing
// structured entries, rotating the log file, and verifying entries exist in
// both the rotated and active files.
func TestTimberjackIntegration(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	logPath := filepath.Join(dir, "slogcp-rolling.log")

	rolling := &timberjack.Logger{
		Filename:         logPath,
		MaxSize:          1,
		MaxBackups:       3,
		MaxAge:           7,
		Compression:      "gzip",
		RotationInterval: 24 * time.Hour,
	}
	rollingClosed := false
	t.Cleanup(func() {
		if rollingClosed {
			return
		}
		if err := rolling.Close(); err != nil {
			t.Errorf("timberjack close: %v", err)
		}
	})

	handler, err := slogcp.NewHandler(nil,
		slogcp.WithRedirectWriter(rolling),
		slogcp.WithSeverityAliases(false),
		slogcp.WithLevel(slog.LevelInfo),
	)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	handlerClosed := false
	t.Cleanup(func() {
		if handlerClosed {
			return
		}
		if cerr := handler.Close(); cerr != nil {
			t.Errorf("handler close: %v", cerr)
		}
	})

	logger := slog.New(handler)

	logger.Info("rotating file logger ready", slog.String("path", rolling.Filename))
	for i := 0; i < 5; i++ {
		logger.Info("processing event", slog.Int("index", i))
	}

	if err := rolling.RotateWithReason("manual"); err != nil {
		t.Fatalf("RotateWithReason: %v", err)
	}
	logger.Info("log rotation complete")

	if err := handler.Close(); err != nil {
		t.Fatalf("handler close: %v", err)
	}
	handlerClosed = true

	if err := rolling.Close(); err != nil {
		t.Fatalf("timberjack close: %v", err)
	}
	rollingClosed = true

	rotatedPath := findRotatedFile(t, dir, filepath.Base(logPath))
	rotatedData := readMaybeGZIP(t, rotatedPath)
	rotatedEntries := decodeEntries(t, rotatedData)
	if len(rotatedEntries) == 0 {
		t.Fatalf("expected rotated log entries")
	}

	lastRotated := rotatedEntries[len(rotatedEntries)-1]
	if msg := lastRotated["message"]; msg != "processing event" {
		t.Fatalf("rotated message = %v, want %q", msg, "processing event")
	}
	idx, ok := lastRotated["index"].(float64)
	if !ok || idx != 4 {
		t.Fatalf("rotated index = %v, want %d", lastRotated["index"], 4)
	}
	if severity := lastRotated["severity"]; severity != "INFO" {
		t.Fatalf("rotated severity = %v, want %q", severity, "INFO")
	}

	currentData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read current log: %v", err)
	}
	currentEntries := decodeEntries(t, currentData)
	if len(currentEntries) == 0 {
		t.Fatalf("expected entry in current log file")
	}

	lastCurrent := currentEntries[len(currentEntries)-1]
	if msg := lastCurrent["message"]; msg != "log rotation complete" {
		t.Fatalf("current message = %v, want %q", msg, "log rotation complete")
	}
	if severity := lastCurrent["severity"]; severity != "INFO" {
		t.Fatalf("current severity = %v, want %q", severity, "INFO")
	}
}

// findRotatedFile locates a rotated log file matching the base name.
func findRotatedFile(t *testing.T, dir, baseName string) string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, entry := range entries {
		if entry.Name() == baseName {
			continue
		}
		return filepath.Join(dir, entry.Name())
	}
	t.Fatalf("rotated file not found")
	return ""
}

// readMaybeGZIP reads a file and transparently decompresses gzip payloads.
func readMaybeGZIP(t *testing.T, path string) []byte {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file %q: %v", path, err)
	}

	reader, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return raw
	}
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read gzip %q: %v", path, err)
	}
	return data
}

// decodeEntries unmarshals newline-delimited JSON log entries from data.
func decodeEntries(t *testing.T, data []byte) []map[string]any {
	t.Helper()

	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil
	}

	lines := bytes.Split(trimmed, []byte("\n"))
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
