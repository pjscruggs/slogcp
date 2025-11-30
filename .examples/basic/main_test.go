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

// TestGoogleCloudLoggerEmitsInfo validates the basic Cloud Run oriented
// example by ensuring the logger emits the expected structured entry.
func TestGoogleCloudLoggerEmitsInfo(t *testing.T) {
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

	slog.New(h).Info("service ready")

	entry := decodeLatestEntry(t, &buf)
	if got := entry["message"]; got != "service ready" {
		t.Fatalf("message = %v, want %q", got, "service ready")
	}
}

// decodeLatestEntry unmarshals the final JSON log line from buf.
func decodeLatestEntry(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()

	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	if len(lines) == 0 {
		t.Fatalf("expected log output")
	}

	var entry map[string]any
	if err := json.Unmarshal(lines[len(lines)-1], &entry); err != nil {
		t.Fatalf("unmarshal log entry: %v", err)
	}
	return entry
}
