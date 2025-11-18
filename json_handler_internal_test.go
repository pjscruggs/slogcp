package slogcp

import (
	"io"
	"log/slog"
	"runtime"
	"testing"
	"time"
)

// TestJSONHandlerResolveSourceLocation exercises the enabled/disabled branches.
func TestJSONHandlerResolveSourceLocation(t *testing.T) {
	t.Parallel()

	baseLogger := slog.New(slog.NewTextHandler(io.Discard, nil))

	disabled := newJSONHandler(&handlerConfig{
		AddSource: false,
		Writer:    io.Discard,
	}, slog.LevelInfo, baseLogger)

	record := slog.NewRecord(time.Now(), slog.LevelInfo, "event", 0)
	if loc := disabled.resolveSourceLocation(record); loc != nil {
		t.Fatalf("expected nil source location when AddSource disabled")
	}

	pc, _, _, _ := runtime.Caller(0)
	record = slog.NewRecord(time.Now(), slog.LevelInfo, "event", pc)

	enabled := newJSONHandler(&handlerConfig{
		AddSource: true,
		Writer:    io.Discard,
	}, slog.LevelInfo, baseLogger)

	loc := enabled.resolveSourceLocation(record)
	if loc == nil {
		t.Fatalf("expected source location when AddSource enabled")
	}
	if loc.File == "" || loc.Function == "" || loc.Line == 0 {
		t.Fatalf("incomplete source location: %+v", loc)
	}

	fallback := slog.NewRecord(time.Now(), slog.LevelInfo, "fallback", pc)
	if src := fallback.Source(); src != nil {
		// Force the handler down the runtime.CallersFrames path.
		src.File = ""
		src.Function = ""
	}
	if loc := enabled.resolveSourceLocation(fallback); loc == nil {
		t.Fatalf("expected fallback source location when Source lacks metadata")
	}

	missingPC := slog.NewRecord(time.Now(), slog.LevelInfo, "missing-pc", 0)
	if loc := enabled.resolveSourceLocation(missingPC); loc != nil {
		t.Fatalf("expected nil source location when PC is zero, got %+v", loc)
	}
}

// TestPruneEmptyMapsRemovesEntries ensures empty nested maps vanish from payloads.
func TestPruneEmptyMapsRemovesEntries(t *testing.T) {
	t.Parallel()

	payload := map[string]any{
		"request": map[string]any{
			"keep": map[string]any{"id": 42},
			"drop": map[string]any{},
		},
		"logging.googleapis.com/labels": map[string]string{},
	}

	pruneEmptyMaps(payload)

	if _, ok := payload["logging.googleapis.com/labels"]; ok {
		t.Fatalf("expected empty labels map to be pruned: %v", payload)
	}

	request, ok := payload["request"].(map[string]any)
	if !ok {
		t.Fatalf("request group missing: %v", payload)
	}
	if _, ok := request["drop"]; ok {
		t.Fatalf("empty nested map was not pruned: %v", request)
	}
	if _, ok := request["keep"]; !ok {
		t.Fatalf("non-empty nested map removed unexpectedly: %v", request)
	}
}
