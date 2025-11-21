package http

import (
	"io"
	"log/slog"
	"testing"
)

// TestWithLoggerHandlesNil verifies WithLogger falls back to slog.Default().
func TestWithLoggerHandlesNil(t *testing.T) {
	t.Parallel()

	cfg := applyOptions([]Option{WithLogger(nil)})
	if cfg.logger != slog.Default() {
		t.Fatalf("WithLogger(nil) did not restore slog.Default()")
	}

	custom := slog.New(slog.NewJSONHandler(io.Discard, nil))
	cfg = applyOptions([]Option{WithLogger(custom)})
	if cfg.logger != custom {
		t.Fatalf("WithLogger custom logger mismatch")
	}
}

// TestWithHTTPRequestAttr toggles automatic httpRequest enrichment flag.
func TestWithHTTPRequestAttr(t *testing.T) {
	t.Parallel()

	cfg := applyOptions([]Option{WithHTTPRequestAttr(true)})
	if !cfg.includeHTTPRequestAttr {
		t.Fatalf("includeHTTPRequestAttr = false, want true")
	}

	cfg = applyOptions([]Option{WithHTTPRequestAttr(false)})
	if cfg.includeHTTPRequestAttr {
		t.Fatalf("includeHTTPRequestAttr = true, want false")
	}
}
