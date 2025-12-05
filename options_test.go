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

package slogcp

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
)

// TestWithLevelControlsLogging ensures log level options filter lower-severity records.
func TestWithLevelControlsLogging(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	h, err := NewHandler(io.Discard,
		WithRedirectWriter(&buf),
		WithLevel(slog.LevelWarn),
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
	h, err := NewHandler(io.Discard,
		WithRedirectWriter(&buf),
		WithAttrs([]slog.Attr{slog.String("global", "yes")}),
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
	h, err := NewHandler(io.Discard,
		WithRedirectWriter(&buf),
		WithGroup("service"),
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
	h, err := NewHandler(io.Discard,
		WithRedirectWriter(&buf),
		WithGroup("service"),
		WithGroup("auth"),
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

// TestWithGroupTrimsWhitespace ensures group names are trimmed before use.
func TestWithGroupTrimsWhitespace(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	h, err := NewHandler(io.Discard,
		WithRedirectWriter(&buf),
		WithGroup("  service  "),
	)
	if err != nil {
		t.Fatalf("NewHandler() returned %v, want nil", err)
	}
	t.Cleanup(func() {
		if cerr := h.Close(); cerr != nil {
			t.Errorf("Handler.Close() returned %v, want nil", cerr)
		}
	})

	slog.New(h).Info("trimmed", slog.String("name", "worker"))

	var payload map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &payload); err != nil {
		t.Fatalf("json.Unmarshal() returned %v", err)
	}
	if _, ok := payload["service"]; !ok {
		t.Fatalf("trimmed group missing from payload: %v", payload)
	}
	if _, ok := payload["  service  "]; ok {
		t.Fatalf("payload unexpectedly contains untrimmed group key: %v", payload)
	}
}

// TestWithGroupClearsWhenBlank ensures blank options clear any prior grouping.
func TestWithGroupClearsWhenBlank(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	h, err := NewHandler(io.Discard,
		WithRedirectWriter(&buf),
		WithGroup("parent"),
		WithGroup("   "), // clears groups
	)
	if err != nil {
		t.Fatalf("NewHandler() returned %v, want nil", err)
	}
	t.Cleanup(func() {
		if cerr := h.Close(); cerr != nil {
			t.Errorf("Handler.Close() returned %v, want nil", cerr)
		}
	})

	slog.New(h).Info("cleared", slog.String("child", "value"))

	var payload map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &payload); err != nil {
		t.Fatalf("json.Unmarshal() returned %v", err)
	}
	if _, ok := payload["parent"]; ok {
		t.Fatalf("parent group should be cleared in payload: %v", payload)
	}
	if got := payload["child"]; got != "value" {
		t.Fatalf("child attribute = %v, want %q", got, "value")
	}
}

// TestWithGroupOrdersStaticAttrs ensures WithAttrs captures the group stack present
// at the time it is invoked instead of being retroactively wrapped by later WithGroup
// options.
func TestWithGroupOrdersStaticAttrs(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	h, err := NewHandler(io.Discard,
		WithRedirectWriter(&buf),
		WithGroup("service"),
		WithAttrs([]slog.Attr{slog.String("svc_attr", "v1")}),
		WithGroup("request"),
		WithAttrs([]slog.Attr{slog.String("req_attr", "v2")}),
	)
	if err != nil {
		t.Fatalf("NewHandler() returned %v, want nil", err)
	}
	t.Cleanup(func() {
		if cerr := h.Close(); cerr != nil {
			t.Errorf("Handler.Close() returned %v, want nil", cerr)
		}
	})

	slog.New(h).Info("ordered")

	var payload map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &payload); err != nil {
		t.Fatalf("json.Unmarshal() returned %v", err)
	}

	serviceGroup, ok := payload["service"].(map[string]any)
	if !ok {
		t.Fatalf("service group missing or wrong type: %T", payload["service"])
	}
	if got := serviceGroup["svc_attr"]; got != "v1" {
		t.Fatalf("svc_attr = %v, want v1", got)
	}
	requestGroup, ok := serviceGroup["request"].(map[string]any)
	if !ok {
		t.Fatalf("request subgroup missing or wrong type: %T", serviceGroup["request"])
	}
	if got := requestGroup["req_attr"]; got != "v2" {
		t.Fatalf("req_attr = %v, want v2", got)
	}
	if _, wrapped := requestGroup["svc_attr"]; wrapped {
		t.Fatalf("svc_attr should remain at service level, got wrapped inside request: %#v", requestGroup)
	}
}

// TestWithAttrsSkipsEmptySlices ensures empty slices do not mutate options state.
func TestWithAttrsSkipsEmptySlices(t *testing.T) {
	t.Parallel()

	opt := WithAttrs(nil)
	opts := &options{}
	opt(opts)
	if len(opts.attrs) != 0 {
		t.Fatalf("opts.attrs length = %d, want 0", len(opts.attrs))
	}

	opt = WithAttrs([]slog.Attr{})
	opt(opts)
	if len(opts.attrs) != 0 {
		t.Fatalf("opts.attrs should remain empty for blank slices")
	}
}

// TestWithAttrsCopiesInput verifies the option stores its own copy of the provided attributes.
func TestWithAttrsCopiesInput(t *testing.T) {
	t.Parallel()

	input := []slog.Attr{slog.String("mutable", "before")}
	opts := &options{}
	WithAttrs(input)(opts)

	input[0] = slog.String("mutable", "after")

	if len(opts.attrs) != 1 || len(opts.attrs[0]) != 1 {
		t.Fatalf("opts.attrs not populated as expected: %#v", opts.attrs)
	}
	if got := opts.attrs[0][0].Value.String(); got != "before" {
		t.Fatalf("copied attribute = %q, want %q", got, "before")
	}
}
