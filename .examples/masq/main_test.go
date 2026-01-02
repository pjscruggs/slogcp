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
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestServerRedactsAuthorizationToken verifies that request logging replaces
// authorization tokens with a redacted placeholder before emission.
func TestServerRedactsAuthorizationToken(t *testing.T) {
	var logBuffer bytes.Buffer
	logger, cleanup, err := newExampleLogger(&logBuffer)
	if err != nil {
		t.Fatalf("newExampleLogger: %v", err)
	}
	defer func() {
		if cleanup != nil {
			if err := cleanup(); err != nil {
				t.Errorf("cleanup logger: %v", err)
			}
		}
	}()

	ts := httptest.NewServer(newRouter(logger))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("health check request: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected health status: %d", resp.StatusCode)
	}

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer secret-token")

	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("hello request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected hello status: %d", resp.StatusCode)
	}

	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["message"] != "hello world" {
		t.Fatalf("unexpected message: %q", body["message"])
	}

	logLines := bytes.Split(bytes.TrimSpace(logBuffer.Bytes()), []byte("\n"))
	if len(logLines) == 0 {
		t.Fatalf("expected log output")
	}

	var entry map[string]any
	if err := json.Unmarshal(logLines[len(logLines)-1], &entry); err != nil {
		t.Fatalf("decode log entry: %v", err)
	}

	if entry["message"] != "hello world" {
		t.Fatalf("unexpected log message: %v", entry["message"])
	}

	token, ok := entry["token"]
	if !ok {
		t.Fatalf("log entry missing token field: %+v", entry)
	}
	if token != "[REDACTED]" {
		t.Fatalf("expected token to be [REDACTED], got %v", token)
	}
}
