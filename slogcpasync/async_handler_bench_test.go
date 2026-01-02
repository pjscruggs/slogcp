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

package slogcpasync

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

// benchNopHandler is a lightweight slog.Handler used to isolate async overhead.
type benchNopHandler struct{}

// Enabled returns true for all records to avoid filtering during benchmarks.
func (benchNopHandler) Enabled(context.Context, slog.Level) bool { return true }

// Handle performs no work to measure async overhead only.
func (benchNopHandler) Handle(context.Context, slog.Record) error { return nil }

// WithAttrs returns a fresh benchNopHandler to satisfy slog.Handler.
func (benchNopHandler) WithAttrs([]slog.Attr) slog.Handler { return benchNopHandler{} }

// WithGroup returns a fresh benchNopHandler to satisfy slog.Handler.
func (benchNopHandler) WithGroup(string) slog.Handler { return benchNopHandler{} }

// withoutWorkers prevents worker goroutines from starting so the queue stays full.
func withoutWorkers() Option {
	return func(cfg *Config) {
		cfg.workerStarter = func(func()) {}
	}
}

// benchmarkHandle drives a handler with RunParallel for throughput measurements.
func benchmarkHandle(b *testing.B, handler slog.Handler, rec slog.Record) {
	ctx := context.Background()

	if c, ok := handler.(interface{ Close() error }); ok {
		b.Cleanup(func() { _ = c.Close() })
	}

	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = handler.Handle(ctx, rec)
		}
	})
}

// BenchmarkHandleSyncBaseline measures the cost of the nop handler without async wrapping.
func BenchmarkHandleSyncBaseline(b *testing.B) {
	rec := slog.NewRecord(time.Time{}, slog.LevelInfo, "bench", 0)
	benchmarkHandle(b, benchNopHandler{}, rec)
}

// BenchmarkHandleAsyncThroughput measures async handler throughput under blocking drop mode.
func BenchmarkHandleAsyncThroughput(b *testing.B) {
	tests := []struct {
		name string
		opts []Option
	}{
		{name: "Queue64_Workers1", opts: []Option{WithQueueSize(64), WithWorkerCount(1)}},
		{name: "Queue64_Workers2", opts: []Option{WithQueueSize(64), WithWorkerCount(2)}},
		{name: "Queue64_Workers4_Batch4", opts: []Option{WithQueueSize(64), WithWorkerCount(4), WithBatchSize(4)}},
		{name: "Queue64_Workers8_Batch4", opts: []Option{WithQueueSize(64), WithWorkerCount(8), WithBatchSize(4)}},
		{name: "Queue1K_Workers1", opts: []Option{WithQueueSize(1024), WithWorkerCount(1)}},
		{name: "Queue1K_Workers2", opts: []Option{WithQueueSize(1024), WithWorkerCount(2)}},
		{name: "Queue1K_Workers4_Batch4", opts: []Option{WithQueueSize(1024), WithWorkerCount(4), WithBatchSize(4)}},
		{name: "Queue1K_Workers8_Batch4", opts: []Option{WithQueueSize(1024), WithWorkerCount(8), WithBatchSize(4)}},
		{name: "Queue8K_Workers4_Batch8", opts: []Option{WithQueueSize(8192), WithWorkerCount(4), WithBatchSize(8)}},
		{name: "Queue8K_Workers8_Batch8", opts: []Option{WithQueueSize(8192), WithWorkerCount(8), WithBatchSize(8)}},
	}

	for _, tt := range tests {
		b.Run(tt.name, func(b *testing.B) {
			handler := Wrap(benchNopHandler{}, tt.opts...)
			rec := slog.NewRecord(time.Time{}, slog.LevelInfo, "bench", 0)
			benchmarkHandle(b, handler, rec)
		})
	}
}

// BenchmarkHandleAsyncDropModes measures drop strategies under immediate saturation.
func BenchmarkHandleAsyncDropModes(b *testing.B) {
	tests := []struct {
		name string
		opts []Option
	}{
		{
			name: "DropNewest_NoWorkers",
			opts: []Option{
				WithQueueSize(1),
				WithDropMode(DropModeDropNewest),
				withoutWorkers(),
			},
		},
		{
			name: "DropOldest_NoWorkers",
			opts: []Option{
				WithQueueSize(1),
				WithDropMode(DropModeDropOldest),
				withoutWorkers(),
			},
		},
		{
			name: "DropNewest_OnDropHook",
			opts: []Option{
				WithQueueSize(1),
				WithDropMode(DropModeDropNewest),
				withoutWorkers(),
				WithOnDrop(func(context.Context, slog.Record) {}),
			},
		},
		{
			name: "DropOldest_OnDropHook",
			opts: []Option{
				WithQueueSize(1),
				WithDropMode(DropModeDropOldest),
				withoutWorkers(),
				WithOnDrop(func(context.Context, slog.Record) {}),
			},
		},
	}

	for _, tt := range tests {
		b.Run(tt.name, func(b *testing.B) {
			handler := Wrap(benchNopHandler{}, tt.opts...)
			rec := slog.NewRecord(time.Time{}, slog.LevelInfo, "bench", 0)
			benchmarkHandle(b, handler, rec)
		})
	}
}

// BenchmarkHandleAsyncPayloadSizes measures impact of attribute density on async throughput.
func BenchmarkHandleAsyncPayloadSizes(b *testing.B) {
	heavyRecord := func() slog.Record {
		rec := slog.NewRecord(time.Time{}, slog.LevelInfo, "heavy", 0)
		rec.AddAttrs(
			slog.String("k1", "v1"),
			slog.String("k2", "v2"),
			slog.String("k3", "v3"),
			slog.Int("k4", 4),
			slog.Int("k5", 5),
			slog.Int("k6", 6),
			slog.Bool("k7", true),
			slog.Bool("k8", false),
			slog.Time("k9", time.Unix(0, 0)),
			slog.Float64("k10", 10.10),
			slog.Float64("k11", 11.11),
			slog.String("k12", "v12"),
			slog.String("k13", "v13"),
			slog.String("k14", "v14"),
			slog.String("k15", "v15"),
			slog.String("k16", "v16"),
		)
		return rec
	}

	cases := []struct {
		name string
		rec  slog.Record
	}{
		{name: "NoAttrs", rec: slog.NewRecord(time.Time{}, slog.LevelInfo, "bench", 0)},
		{name: "HeavyAttrs", rec: heavyRecord()},
	}

	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			handler := Wrap(benchNopHandler{},
				WithQueueSize(1024),
				WithWorkerCount(4),
				WithDropMode(DropModeBlock),
			)
			benchmarkHandle(b, handler, tc.rec)
		})
	}
}
