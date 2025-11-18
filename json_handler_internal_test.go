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

	origSourceFunc := recordSourceFunc
	origFrameResolver := runtimeFrameResolver
	t.Cleanup(func() {
		recordSourceFunc = origSourceFunc
		runtimeFrameResolver = origFrameResolver
	})

	recordSourceFunc = func(slog.Record) *slog.Source {
		return &slog.Source{}
	}
	runtimeFrameResolver = func(uintptr) runtime.Frame {
		return runtime.Frame{
			File:     "fallback.go",
			Line:     123,
			Function: "fallback",
		}
	}
	fallbackRecord := slog.NewRecord(time.Now(), slog.LevelInfo, "needs-fallback", pc)
	if loc := enabled.resolveSourceLocation(fallbackRecord); loc == nil || loc.Function != "fallback" {
		t.Fatalf("expected fallback data, got %+v", loc)
	}

	recordSourceFunc = func(slog.Record) *slog.Source { return &slog.Source{} }
	runtimeFrameResolver = func(uintptr) runtime.Frame { return runtime.Frame{} }
	if loc := enabled.resolveSourceLocation(fallbackRecord); loc != nil {
		t.Fatalf("expected nil when frame lacks metadata, got %+v", loc)
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

// TestPayloadStatePrepareResetsUsedMaps ensures prepare clears root maps and usage tracking.
func TestPayloadStatePrepareResetsUsedMaps(t *testing.T) {
	t.Parallel()

	var ps payloadState
	root := ps.prepare(1)
	root["foo"] = "bar"

	aux := ps.borrowMap(2)
	aux["scratch"] = 1
	if len(ps.usedMaps) != 1 {
		t.Fatalf("usedMaps = %d, want 1", len(ps.usedMaps))
	}

	root = ps.prepare(8)
	if len(root) != 0 {
		t.Fatalf("prepare did not clear root map: %v", root)
	}
	if len(ps.usedMaps) != 0 {
		t.Fatalf("usedMaps = %d after prepare, want 0", len(ps.usedMaps))
	}
}

// TestPayloadStateObtainLabelsClears verifies the labels map is reused and cleared.
func TestPayloadStateObtainLabelsClears(t *testing.T) {
	t.Parallel()

	var ps payloadState
	labels := ps.obtainLabels()
	labels["env"] = "prod"

	reused := ps.obtainLabels()
	if len(reused) != 0 {
		t.Fatalf("labels not cleared, len=%d", len(reused))
	}
	if _, exists := reused["env"]; exists {
		t.Fatalf("stale label entries persisted: %v", reused)
	}
}

// TestPayloadStateBorrowMapRecycle exercises both allocation paths and recycling.
func TestPayloadStateBorrowMapRecycle(t *testing.T) {
	t.Parallel()

	var ps payloadState
	first := ps.borrowMap(1)
	first["alpha"] = "a"
	second := ps.borrowMap(3)
	second["beta"] = "b"

	if len(ps.usedMaps) != 2 {
		t.Fatalf("usedMaps = %d, want 2", len(ps.usedMaps))
	}
	if len(ps.freeMaps) != 0 {
		t.Fatalf("freeMaps = %d before recycle, want 0", len(ps.freeMaps))
	}

	ps.recycle()
	if len(ps.usedMaps) != 0 {
		t.Fatalf("usedMaps = %d after recycle, want 0", len(ps.usedMaps))
	}
	if len(ps.freeMaps) != 2 {
		t.Fatalf("freeMaps = %d after recycle, want 2", len(ps.freeMaps))
	}

	reused := ps.borrowMap(1)
	if len(reused) != 0 {
		t.Fatalf("reused map was not cleared: %v", reused)
	}
	if len(ps.freeMaps) != 1 {
		t.Fatalf("freeMaps = %d after borrowing recycled map, want 1", len(ps.freeMaps))
	}
}
