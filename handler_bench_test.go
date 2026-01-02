// Copyright 2025-2026 Patrick J. Scruggs
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
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/pjscruggs/slogcp/slogcpasync"
)

// BenchmarkHandlerAsyncModes compares sync/async handler paths for common targets.
func BenchmarkHandlerAsyncModes(b *testing.B) {
	benchTime := time.Unix(1700000000, 0).UTC()
	rec := slog.NewRecord(benchTime, slog.LevelInfo, "bench", 0)
	rec.AddAttrs(
		slog.String("request_id", "abc123"),
		slog.Int("attempt", 1),
		slog.String("region", "us-central1"),
	)

	ctx := context.Background()

	b.Run("NonFileSync", func(b *testing.B) {
		handler := newBenchHandler(b, io.Discard)
		benchmarkHandlerHandle(b, handler, ctx, rec)
	})

	b.Run("NonFileAsync", func(b *testing.B) {
		handler := newBenchHandler(
			b,
			io.Discard,
			WithAsync(
				slogcpasync.WithQueueSize(1024),
				slogcpasync.WithWorkerCount(4),
				slogcpasync.WithBatchSize(4),
			),
		)
		benchmarkHandlerHandle(b, handler, ctx, rec)
	})

	b.Run("FileTargetAsyncDefault", func(b *testing.B) {
		path := filepath.Join(b.TempDir(), "bench.log")
		handler := newBenchHandler(b, nil, WithRedirectToFile(path))
		benchmarkHandlerHandle(b, handler, ctx, rec)
	})

	b.Run("FileTargetSyncForced", func(b *testing.B) {
		path := filepath.Join(b.TempDir(), "bench.log")
		handler := newBenchHandler(
			b,
			nil,
			WithRedirectToFile(path),
			WithAsyncOnFile(slogcpasync.WithEnabled(false)),
		)
		benchmarkHandlerHandle(b, handler, ctx, rec)
	})
}

// BenchmarkHandlerParallel measures handler throughput under concurrent logging.
func BenchmarkHandlerParallel(b *testing.B) {
	benchTime := time.Unix(1700000000, 0).UTC()
	rec := slog.NewRecord(benchTime, slog.LevelInfo, "bench", 0)
	rec.AddAttrs(
		slog.String("request_id", "abc123"),
		slog.Int("attempt", 1),
		slog.Bool("cached", true),
	)

	ctx := context.Background()

	b.Run("Sync", func(b *testing.B) {
		handler := newBenchHandler(b, io.Discard)
		benchmarkHandlerParallel(b, handler, ctx, rec)
	})

	b.Run("Async", func(b *testing.B) {
		handler := newBenchHandler(
			b,
			io.Discard,
			WithAsync(
				slogcpasync.WithQueueSize(1024),
				slogcpasync.WithWorkerCount(4),
				slogcpasync.WithBatchSize(4),
			),
		)
		benchmarkHandlerParallel(b, handler, ctx, rec)
	})
}

// newBenchHandler builds a slogcp handler and registers cleanup for benchmarks.
func newBenchHandler(b *testing.B, writer io.Writer, opts ...Option) slog.Handler {
	b.Helper()

	handler, err := NewHandler(writer, opts...)
	if err != nil {
		b.Fatalf("NewHandler returned error: %v", err)
	}
	b.Cleanup(func() {
		if err := handler.Close(); err != nil {
			b.Fatalf("handler.Close returned error: %v", err)
		}
	})
	return handler
}

// benchmarkHandlerHandle drives handler.Handle with allocation reporting.
func benchmarkHandlerHandle(b *testing.B, handler slog.Handler, ctx context.Context, rec slog.Record) {
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		if err := handler.Handle(ctx, rec); err != nil {
			b.Fatalf("Handle returned error: %v", err)
		}
	}
}

// benchmarkHandlerParallel drives handler.Handle under RunParallel.
func benchmarkHandlerParallel(b *testing.B, handler slog.Handler, ctx context.Context, rec slog.Record) {
	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if err := handler.Handle(ctx, rec); err != nil {
				b.Fatalf("Handle returned error: %v", err)
			}
		}
	})
}
