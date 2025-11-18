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

package slogcp_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/pjscruggs/slogcp"
)

// TestWithLevelControlsLogging ensures log level options filter lower-severity records.
func TestWithLevelControlsLogging(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	h, err := slogcp.NewHandler(io.Discard,
		slogcp.WithRedirectWriter(&buf),
		slogcp.WithLevel(slog.LevelWarn),
	)
	if err != nil {
		t.Fatalf("NewHandler() returned %v, want nil", err)
	}
	t.Cleanup(func() {
		if cerr := h.Close(); cerr != nil {
			t.Errorf("Handler.Close() returned %v, want nil", cerr)
		}
	})

	logger := slog.New(h)
	logger.Info("suppressed")
	logger.Error("emitted")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 log line, got %d: %q", len(lines), buf.String())
	}
	if !strings.Contains(lines[0], `"message":"emitted"`) {
		t.Fatalf("unexpected log line: %q", lines[0])
	}
}

// TestWithAttrsAddsStaticAttributes verifies WithAttrs attaches static values to every record.
func TestWithAttrsAddsStaticAttributes(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	h, err := slogcp.NewHandler(io.Discard,
		slogcp.WithRedirectWriter(&buf),
		slogcp.WithAttrs([]slog.Attr{slog.String("global", "yes")}),
	)
	if err != nil {
		t.Fatalf("NewHandler() returned %v, want nil", err)
	}
	t.Cleanup(func() {
		if cerr := h.Close(); cerr != nil {
			t.Errorf("Handler.Close() returned %v, want nil", cerr)
		}
	})

	slog.New(h).Info("attrs")

	var payload map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &payload); err != nil {
		t.Fatalf("json.Unmarshal() returned %v", err)
	}
	if got := payload["global"]; got != "yes" {
		t.Fatalf("global attribute = %v, want %q", got, "yes")
	}
}

// TestWithGroupNestsAttributes confirms WithGroup nests attributes under the chosen key.
func TestWithGroupNestsAttributes(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	h, err := slogcp.NewHandler(io.Discard,
		slogcp.WithRedirectWriter(&buf),
		slogcp.WithGroup("service"),
	)
	if err != nil {
		t.Fatalf("NewHandler() returned %v, want nil", err)
	}
	t.Cleanup(func() {
		if cerr := h.Close(); cerr != nil {
			t.Errorf("Handler.Close() returned %v, want nil", cerr)
		}
	})

	slog.New(h).Info("grouped", slog.String("name", "api"))

	var payload map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &payload); err != nil {
		t.Fatalf("json.Unmarshal() returned %v", err)
	}
	groupAny, ok := payload["service"]
	if !ok {
		t.Fatalf("service group missing from payload: %v", payload)
	}
	groupMap, ok := groupAny.(map[string]any)
	if !ok {
		t.Fatalf("service attribute type = %T, want map[string]any", groupAny)
	}
	if got := groupMap["name"]; got != "api" {
		t.Fatalf("group attribute name = %v, want %q", got, "api")
	}
}

// TestWithGroupAccumulatesOptions ensures multiple WithGroup options compose instead of overwriting.
func TestWithGroupAccumulatesOptions(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	h, err := slogcp.NewHandler(io.Discard,
		slogcp.WithRedirectWriter(&buf),
		slogcp.WithGroup("service"),
		slogcp.WithGroup("auth"),
	)
	if err != nil {
		t.Fatalf("NewHandler() returned %v, want nil", err)
	}
	t.Cleanup(func() {
		if cerr := h.Close(); cerr != nil {
			t.Errorf("Handler.Close() returned %v, want nil", cerr)
		}
	})

	slog.New(h).Info("nested", slog.String("tenant", "acme"))

	var payload map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &payload); err != nil {
		t.Fatalf("json.Unmarshal() returned %v", err)
	}
	serviceAny, ok := payload["service"]
	if !ok {
		t.Fatalf("service group missing from payload: %v", payload)
	}
	serviceMap, ok := serviceAny.(map[string]any)
	if !ok {
		t.Fatalf("service group type = %T, want map[string]any", serviceAny)
	}
	authAny, ok := serviceMap["auth"]
	if !ok {
		t.Fatalf("auth subgroup missing from payload: %v", serviceMap)
	}
	authMap, ok := authAny.(map[string]any)
	if !ok {
		t.Fatalf("auth subgroup type = %T, want map[string]any", authAny)
	}
	if got := authMap["tenant"]; got != "acme" {
		t.Fatalf("nested group tenant = %v, want %q", got, "acme")
	}
}
