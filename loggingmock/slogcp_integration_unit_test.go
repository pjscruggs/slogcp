//go:build unit
// +build unit

package loggingmock

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/pjscruggs/slogcp"
)

// TestSlogcpLogEntryTransforms exercises slogcp JSON output against the logging mock transformer.
func TestSlogcpLogEntryTransforms(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	h, err := slogcp.NewHandler(io.Discard, slogcp.WithRedirectWriter(&buf))
	if err != nil {
		t.Fatalf("NewHandler() returned %v, want nil", err)
	}
	t.Cleanup(func() {
		if cerr := h.Close(); cerr != nil {
			t.Errorf("Handler.Close() returned %v, want nil", cerr)
		}
	})

	logger := slog.New(h)
	req := &slogcp.HTTPRequest{
		RequestMethod:  "GET",
		RequestURL:     "https://example.com/orders/123",
		RequestSize:    128,
		Status:         200,
		ResponseSize:   512,
		RemoteIP:       "203.0.113.1",
		LocalIP:        "10.0.0.5",
		UserAgent:      "integration-test/1.0",
		CacheHit:       true,
		CacheLookup:    true,
		CacheFillBytes: 64,
	}

	logger.WarnContext(
		context.Background(),
		"order failed",
		slog.Int("attempt", 3),
		slog.Any("httpRequest", req),
		slog.Group(
			slogcp.LabelsGroup,
			slog.String("component", "payments"),
			slog.String("region", "us-central1"),
		),
	)

	line := strings.TrimSpace(buf.String())
	if line == "" {
		t.Fatal("expected log output")
	}

	if err := EnsureJSONObject(line); err != nil {
		t.Fatalf("EnsureJSONObject() returned %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		t.Fatalf("json.Unmarshal() returned %v", err)
	}

	if got := raw["severity"]; got != "W" {
		t.Fatalf("raw severity = %v, want W", got)
	}
	if got := raw["message"]; got != "order failed" {
		t.Fatalf("raw message = %v, want order failed", got)
	}

	if got, ok := raw["attempt"].(float64); !ok || got != 3 {
		t.Fatalf("raw attempt = %v (type %T), want float64(3)", raw["attempt"], raw["attempt"])
	}

	rawLabels, ok := raw[slogcp.LabelsGroup].(map[string]any)
	if !ok {
		t.Fatalf("raw labels missing or wrong type: %T", raw[slogcp.LabelsGroup])
	}
	if got := rawLabels["component"]; got != "payments" {
		t.Fatalf("raw labels component = %v, want payments", got)
	}
	if got := rawLabels["region"]; got != "us-central1" {
		t.Fatalf("raw labels region = %v, want us-central1", got)
	}

	rawHTTP, ok := raw["httpRequest"].(map[string]any)
	if !ok {
		t.Fatalf("raw httpRequest missing or wrong type: %T", raw["httpRequest"])
	}
	if got := rawHTTP["requestMethod"]; got != "GET" {
		t.Fatalf("raw requestMethod = %v, want GET", got)
	}
	if got := rawHTTP["requestUrl"]; got != "https://example.com/orders/123" {
		t.Fatalf("raw requestUrl = %v, want https://example.com/orders/123", got)
	}
	if got := rawHTTP["status"]; got != float64(200) {
		t.Fatalf("raw status = %v, want 200", got)
	}
	if got := rawHTTP["requestSize"]; got != "128" {
		t.Fatalf("raw requestSize = %v, want 128", got)
	}
	if got := rawHTTP["responseSize"]; got != "512" {
		t.Fatalf("raw responseSize = %v, want 512", got)
	}

	now := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	transformed, err := TransformLogEntryJSON(line, now)
	if err != nil {
		t.Fatalf("TransformLogEntryJSON() returned %v", err)
	}
	entry := mustUnmarshalMap(t, transformed)

	if got := entry["timestamp"]; got != formatRFC3339ZNormalized(now) {
		t.Fatalf("transformed timestamp = %v, want %s", got, formatRFC3339ZNormalized(now))
	}
	if got := entry["severity"]; got != "WARNING" {
		t.Fatalf("transformed severity = %v, want WARNING", got)
	}
	if got := entry["message"]; got != "order failed" {
		t.Fatalf("transformed message = %v, want order failed", got)
	}

	if got, ok := entry["attempt"].(float64); !ok || got != 3 {
		t.Fatalf("transformed attempt = %v (type %T), want float64(3)", entry["attempt"], entry["attempt"])
	}

	labels, ok := entry[slogcp.LabelsGroup].(map[string]any)
	if !ok {
		t.Fatalf("transformed labels missing or wrong type: %T", entry[slogcp.LabelsGroup])
	}
	if got := labels["component"]; got != "payments" {
		t.Fatalf("transformed labels component = %v, want payments", got)
	}
	if got := labels["region"]; got != "us-central1" {
		t.Fatalf("transformed labels region = %v, want us-central1", got)
	}

	httpReq, ok := entry["httpRequest"].(map[string]any)
	if !ok {
		t.Fatalf("transformed httpRequest missing or wrong type: %T", entry["httpRequest"])
	}
	if got := httpReq["requestMethod"]; got != "GET" {
		t.Fatalf("transformed requestMethod = %v, want GET", got)
	}
	if got := httpReq["requestUrl"]; got != "https://example.com/orders/123" {
		t.Fatalf("transformed requestUrl = %v, want https://example.com/orders/123", got)
	}
	if got := httpReq["status"]; got != float64(200) {
		t.Fatalf("transformed status = %v, want 200", got)
	}
	if got := httpReq["requestSize"]; got != "128" {
		t.Fatalf("transformed requestSize = %v, want 128", got)
	}
	if got := httpReq["responseSize"]; got != "512" {
		t.Fatalf("transformed responseSize = %v, want 512", got)
	}
}
