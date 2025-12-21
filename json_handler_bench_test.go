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
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"
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
	h := newJSONHandler(cfg, slog.LevelInfo, slog.New(slog.DiscardHandler))

	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()

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

// BenchmarkJSONHandlerCore covers common slogcp JSON handler record variants.
func BenchmarkJSONHandlerCore(b *testing.B) {
	benchTime := time.Unix(1700000000, 0).UTC()
	ctx := context.Background()

	traceID, _ := trace.TraceIDFromHex("105445aa7843bc8bf206b12000100000")
	spanID, _ := trace.SpanIDFromHex("09158d8185d3c3af")
	spanCtx := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	})
	ctxWithSpan := trace.ContextWithSpanContext(ctx, spanCtx)

	b.Run("Typical", func(b *testing.B) {
		cfg := benchHandlerConfig()
		h := newJSONHandler(cfg, slog.LevelInfo, slog.New(slog.DiscardHandler))

		rec := slog.NewRecord(benchTime, slog.LevelInfo, "bench", 0)
		rec.AddAttrs(
			slog.String("request_id", "abc123"),
			slog.Int("attempt", 1),
			slog.Bool("cached", true),
			slog.String("region", "us-central1"),
		)

		benchmarkJSONHandle(b, h, ctx, rec)
	})

	b.Run("NestedMixed", func(b *testing.B) {
		cfg := benchHandlerConfig()
		h := newJSONHandler(cfg, slog.LevelInfo, slog.New(slog.DiscardHandler))

		rec := slog.NewRecord(benchTime, slog.LevelInfo, "bench", 0)
		rec.AddAttrs(
			slog.String("user_id", "u-123"),
			slog.Group("http",
				slog.String("method", "POST"),
				slog.Int("status", 202),
				slog.Group("client",
					slog.String("ip", "203.0.113.5"),
					slog.Bool("mobile", false),
				),
			),
			slog.Time("received_at", benchTime),
			slog.Duration("duration", 150*time.Millisecond),
		)

		benchmarkJSONHandle(b, h, ctx, rec)
	})

	b.Run("ErrorStackDisabled", func(b *testing.B) {
		cfg := benchHandlerConfig()
		cfg.StackTraceEnabled = false
		h := newJSONHandler(cfg, slog.LevelInfo, slog.New(slog.DiscardHandler))

		rec := slog.NewRecord(benchTime, slog.LevelError, "bench error", 0)
		rec.AddAttrs(slog.Any("error", errors.New("boom")))

		benchmarkJSONHandle(b, h, ctx, rec)
	})

	b.Run("ErrorStackEnabled", func(b *testing.B) {
		cfg := benchHandlerConfig()
		cfg.StackTraceEnabled = true
		cfg.StackTraceLevel = slog.LevelError
		h := newJSONHandler(cfg, slog.LevelInfo, slog.New(slog.DiscardHandler))

		rec := slog.NewRecord(benchTime, slog.LevelError, "bench error", 0)
		rec.AddAttrs(slog.Any("error", errors.New("boom")))

		benchmarkJSONHandle(b, h, ctx, rec)
	})

	b.Run("ErrorReportingAttrs", func(b *testing.B) {
		cfg := benchHandlerConfig()
		cfg.StackTraceEnabled = false
		h := newJSONHandler(cfg, slog.LevelInfo, slog.New(slog.DiscardHandler))

		err := errors.New("boom")
		errorAttrs := ErrorReportingAttrs(err, WithErrorServiceContext("bench", "v1"))

		rec := slog.NewRecord(benchTime, slog.LevelError, "bench error", 0)
		rec.AddAttrs(append([]slog.Attr{slog.Any("error", err)}, errorAttrs...)...)

		benchmarkJSONHandle(b, h, ctx, rec)
	})

	b.Run("TraceAbsent", func(b *testing.B) {
		cfg := benchHandlerConfig()
		h := newJSONHandler(cfg, slog.LevelInfo, slog.New(slog.DiscardHandler))

		rec := slog.NewRecord(benchTime, slog.LevelInfo, "bench", 0)
		rec.AddAttrs(slog.String("operation", "lookup"))

		benchmarkJSONHandle(b, h, ctx, rec)
	})

	b.Run("TracePresent", func(b *testing.B) {
		cfg := benchHandlerConfig()
		h := newJSONHandler(cfg, slog.LevelInfo, slog.New(slog.DiscardHandler))

		rec := slog.NewRecord(benchTime, slog.LevelInfo, "bench", 0)
		rec.AddAttrs(slog.String("operation", "lookup"))

		benchmarkJSONHandle(b, h, ctxWithSpan, rec)
	})
}

// BenchmarkJSONHandlerAttrProcessing exercises ReplaceAttr and high-attr workloads.
func BenchmarkJSONHandlerAttrProcessing(b *testing.B) {
	benchTime := time.Unix(1700000000, 0).UTC()
	ctx := context.Background()

	b.Run("ReplaceAttrDisabled", func(b *testing.B) {
		cfg := benchHandlerConfig()
		h := newJSONHandler(cfg, slog.LevelInfo, slog.New(slog.DiscardHandler))

		rec := slog.NewRecord(benchTime, slog.LevelInfo, "bench", 0)
		rec.AddAttrs(slog.String("token", "secret"), slog.String("user", "bench"))

		benchmarkJSONHandle(b, h, ctx, rec)
	})

	b.Run("ReplaceAttrEnabled", func(b *testing.B) {
		cfg := benchHandlerConfig()
		cfg.ReplaceAttr = func(_ []string, attr slog.Attr) slog.Attr {
			if attr.Key == "token" {
				return slog.String("token", "[redacted]")
			}
			return attr
		}
		h := newJSONHandler(cfg, slog.LevelInfo, slog.New(slog.DiscardHandler))

		rec := slog.NewRecord(benchTime, slog.LevelInfo, "bench", 0)
		rec.AddAttrs(slog.String("token", "secret"), slog.String("user", "bench"))

		benchmarkJSONHandle(b, h, ctx, rec)
	})

	b.Run("HighAttrCount", func(b *testing.B) {
		cfg := benchHandlerConfig()
		h := newJSONHandler(cfg, slog.LevelInfo, slog.New(slog.DiscardHandler))

		attrs := make([]slog.Attr, 0, 64)
		for i := range 64 {
			key := "k" + strconv.Itoa(i)
			attrs = append(attrs, slog.Int(key, i))
		}

		rec := slog.NewRecord(benchTime, slog.LevelInfo, "bench", 0)
		rec.AddAttrs(attrs...)

		benchmarkJSONHandle(b, h, ctx, rec)
	})
}

// benchHandlerConfig prepares a base handlerConfig for JSON handler benchmarks.
func benchHandlerConfig() *handlerConfig {
	return &handlerConfig{
		Writer:                   io.Discard,
		EmitTimeField:            true,
		TraceProjectID:           "bench-project",
		UseShortSeverityNames:    false,
		StackTraceLevel:          slog.LevelError,
		runtimeServiceContext:    map[string]string{"service": "bench", "version": "v1"},
		runtimeServiceContextAny: map[string]any{"service": "bench", "version": "v1"},
	}
}

// benchmarkJSONHandle runs handler.Handle in a tight loop with allocation reporting.
func benchmarkJSONHandle(b *testing.B, h *jsonHandler, ctx context.Context, rec slog.Record) {
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		if err := h.Handle(ctx, rec); err != nil {
			b.Fatalf("Handle returned error: %v", err)
		}
	}
}
