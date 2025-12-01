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

//go:build benchmarks
package loggingmock

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pjscruggs/slogcp"
	"github.com/pjscruggs/slogcp/slogcpasync"
)

// sinkHandler discards output while exercising Handler.Handle.
func sinkHandler(b *testing.B, async bool, opts ...slogcpasync.Option) slog.Handler {
	// Write to a temp file to simulate real I/O without console spam.
	tmp := filepath.Join(b.TempDir(), "lograte.log")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		b.Fatalf("open output file: %v", err)
	}
	b.Cleanup(func() {
		_ = f.Close()
		_ = os.Remove(tmp)
	})

	h, err := slogcp.NewHandler(f)
	if err != nil {
		b.Fatalf("NewHandler: %v", err)
	}
	if !async {
		return h
	}
	return slogcpasync.Wrap(h, opts...)
}

// benchmarkLogger drives Handler.Handle in parallel for benchmarking.
func benchmarkLogger(b *testing.B, handler slog.Handler) {
	rec := slog.NewRecord(time.Now(), slog.LevelInfo, "bench", 0)
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = handler.Handle(ctx, rec)
		}
	})
	if closer, ok := handler.(interface{ Close() error }); ok {
		_ = closer.Close()
	}
}

// BenchmarkLogRateSync measures the synchronous handler baseline.
func BenchmarkLogRateSync(b *testing.B) {
	h := sinkHandler(b, false)
	benchmarkLogger(b, h)
}

// BenchmarkLogRateAsync measures the async wrapper across queue/worker configs.
func BenchmarkLogRateAsync(b *testing.B) {
	// Allow single-combo benchmarking via env to make auto-tuning fast.
	if szEnv, wEnv, bEnv := os.Getenv("SLOGCP_BENCH_QUEUE"), os.Getenv("SLOGCP_BENCH_WORKERS"), os.Getenv("SLOGCP_BENCH_BATCH"); szEnv != "" && wEnv != "" && bEnv != "" {
		sz, err := strconv.Atoi(szEnv)
		if err != nil {
			b.Fatalf("invalid SLOGCP_BENCH_QUEUE: %v", err)
		}
		w, err := strconv.Atoi(wEnv)
		if err != nil {
			b.Fatalf("invalid SLOGCP_BENCH_WORKERS: %v", err)
		}
		batch, err := strconv.Atoi(bEnv)
		if err != nil {
			b.Fatalf("invalid SLOGCP_BENCH_BATCH: %v", err)
		}
		mode, modeName, err := parseBenchDropMode(os.Getenv("SLOGCP_BENCH_DROP_MODE"))
		if err != nil {
			b.Fatalf("%v", err)
		}

		name := fmt.Sprintf("SZ%d_W%d_B%d_%s", sz, w, batch, modeName)
		b.Run(name, func(b *testing.B) {
			h := sinkHandler(b, true,
				slogcpasync.WithQueueSize(sz),
				slogcpasync.WithWorkerCount(w),
				slogcpasync.WithBatchSize(batch),
				slogcpasync.WithDropMode(mode),
			)
			benchmarkLogger(b, h)
		})
		return
	}

	queueSizes := []int{64, 256, 1024, 4096, 8192}
	workerCounts := []int{1, 2, 4, 8}
	batchSizes := []int{1, 2, 4, 8}

	for _, sz := range queueSizes {
		for _, w := range workerCounts {
			for _, batch := range batchSizes {
				name := fmt.Sprintf("SZ%d_W%d_B%d", sz, w, batch)
				b.Run(name, func(b *testing.B) {
					h := sinkHandler(b, true,
						slogcpasync.WithQueueSize(sz),
						slogcpasync.WithWorkerCount(w),
						slogcpasync.WithBatchSize(batch),
					)
					benchmarkLogger(b, h)
				})
			}
		}
	}
}

// parseBenchDropMode converts the env-provided drop mode into the slogcpasync enum.
func parseBenchDropMode(raw string) (slogcpasync.DropMode, string, error) {
	if raw == "" {
		return slogcpasync.DropModeBlock, "block", nil
	}

	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "block":
		return slogcpasync.DropModeBlock, "block", nil
	case "drop_newest", "drop-newest":
		return slogcpasync.DropModeDropNewest, "drop_newest", nil
	case "drop_oldest", "drop-oldest":
		return slogcpasync.DropModeDropOldest, "drop_oldest", nil
	default:
		return slogcpasync.DropModeBlock, "", fmt.Errorf("invalid SLOGCP_BENCH_DROP_MODE: %q", raw)
	}
}
