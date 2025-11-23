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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pjscruggs/slogcp"
	slogcphttp "github.com/pjscruggs/slogcp/slogcphttp"
)

// TestHTTPMiddlewareLogsRequest covers the HTTP server example by invoking the
// wrapped handler and validating the emitted log entry.
func TestHTTPMiddlewareLogsRequest(t *testing.T) {
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

	handler := slogcphttp.Middleware(
		slogcphttp.WithLogger(logger),
	)(
		slogcphttp.InjectTraceContextMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			logger.InfoContext(r.Context(), "handling request")
			w.WriteHeader(http.StatusNoContent)
		})),
	)

	mux := http.NewServeMux()
	mux.Handle("/api", handler)

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/api")
	if err != nil {
		t.Fatalf("http.Get: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}

	entries := decodeEntries(t, &buf)

	var found bool
	for _, entry := range entries {
		if entry["message"] == "handling request" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected handling request log entry, got %v", entries)
	}
}

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
