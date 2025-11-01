package slogcp

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

// BenchmarkLevelToString exercises levelToString conversions across various slog levels.
func BenchmarkLevelToString(b *testing.B) {
	levels := []slog.Level{
		slog.LevelDebug,
		slog.LevelInfo,
		slog.Level(LevelNotice) - 1,
		slog.Level(LevelNotice),
		slog.LevelWarn,
		slog.LevelError,
		slog.Level(LevelCritical),
		slog.Level(LevelAlert),
		slog.Level(LevelEmergency),
		slog.Level(LevelDefault),
		slog.Level(LevelDefault) + 5,
	}

	for b.Loop() {
		for _, lvl := range levels {
			if severityAliasString(lvl) == "" {
				b.Fatalf("empty level string for %v", lvl)
			}
		}
	}
}

// BenchmarkJSONHandlerHandle measures Handle performance for the JSON handler with typical attributes.
func BenchmarkJSONHandlerHandle(b *testing.B) {
	cfg := &handlerConfig{
		Writer:        io.Discard,
		EmitTimeField: true,
	}
	h := newJSONHandler(cfg, slog.LevelInfo, slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx := context.Background()

	for i := 0; b.Loop(); i++ {
		rec := slog.NewRecord(time.Now(), slog.LevelInfo, "benchmark message", 0)
		rec.AddAttrs(
			slog.String("request_id", "abc123"),
			slog.Int("attempt", i),
			slog.Group("http", slog.String("method", "GET"), slog.Int("status", 200)),
		)
		if err := h.Handle(ctx, rec); err != nil {
			b.Fatalf("Handle returned error: %v", err)
		}
	}
}
