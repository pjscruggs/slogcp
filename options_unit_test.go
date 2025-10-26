//go:build unit
// +build unit

package slogcp_test

import (
	"bytes"
	json "encoding/json/v2"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/pjscruggs/slogcp"
)

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
