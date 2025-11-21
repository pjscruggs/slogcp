package slogcp

import (
	"io"
	"log/slog"
	"testing"
	"time"
)

// TestJSONHandlerCapturesHTTPRequestAttrBeforeResolution ensures buildPayload
// preserves the Cloud Logging httpRequest attribute even when attr values are
// resolved before extraction.
func TestJSONHandlerCapturesHTTPRequestAttrBeforeResolution(t *testing.T) {
	t.Parallel()

	cfg := &handlerConfig{Writer: io.Discard}
	h := newJSONHandler(cfg, slog.LevelInfo, slog.New(slog.NewTextHandler(io.Discard, nil)))
	state := &payloadState{}

	req := &HTTPRequest{RequestMethod: "GET", RequestURL: "https://example.com/items/1"}
	record := slog.NewRecord(time.Now(), slog.LevelInfo, "hello", 0)
	record.AddAttrs(slog.Any("httpRequest", req))

	_, captured, _, _, _, _ := h.buildPayload(record, state)
	if captured == nil {
		t.Fatalf("buildPayload did not capture httpRequest attr")
	}
	if captured.RequestMethod != "GET" || captured.RequestURL != "https://example.com/items/1" {
		t.Fatalf("captured httpRequest mismatch: %+v", captured)
	}
}
