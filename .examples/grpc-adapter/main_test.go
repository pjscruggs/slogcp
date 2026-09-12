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
	"context"
	"encoding/json"
	"testing"
	"time"
)

// TestRun verifies that the external middleware logs an actual completed RPC.
func TestRun(t *testing.T) {
	var output bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := run(ctx, &output); err != nil {
		t.Fatal(err)
	}
	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &entry); err != nil {
		t.Fatalf("decode completion log %v with output %s", err, output.String())
	}
	for key, want := range map[string]any{
		"message":      "finished call",
		"grpc.service": "grpc.health.v1.Health",
		"grpc.method":  "Check",
		"grpc.code":    "OK",
	} {
		if entry[key] != want {
			t.Errorf("%s got %v want %v", key, entry[key], want)
		}
	}
	if _, ok := entry["grpc.time_ms"]; !ok {
		t.Error("completion duration missing")
	}
}
