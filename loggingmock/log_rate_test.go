//go:build benchmarks
// +build benchmarks

package loggingmock

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/pjscruggs/slogcp"
	"github.com/pjscruggs/slogcp/slogcpasync"
)

// sinkHandler discards output while exercising Handler.Handle.
func sinkHandler(b *testing.B, async bool, opts ...slogcpasync.Option) slog.Handler {
	// Discard output to avoid I/O skew in throughput results.
	w := io.Discard
	if envOut := os.Getenv("SLOGCP_BENCH_OUT"); envOut != "" {
		// Allow overriding for debugging.
		f, err := os.OpenFile(envOut, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			b.Fatalf("open output file: %v", err)
		}
		w = f
		b.Cleanup(func() { _ = f.Close() })
	}

	h, err := slogcp.NewHandler(w)
	if err != nil {
		b.Fatalf("NewHandler: %v", err)
	}
	if !async {
		return h
	}
	return slogcpasync.Wrap(h, opts...)
}

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

func BenchmarkLogRateSync(b *testing.B) {
	h := sinkHandler(b, false)
	benchmarkLogger(b, h)
}

func BenchmarkLogRateAsync(b *testing.B) {
	cases := []struct {
		name string
		opts []slogcpasync.Option
	}{
		{name: "Queue1K_W1", opts: []slogcpasync.Option{slogcpasync.WithQueueSize(1024), slogcpasync.WithWorkerCount(1)}},
		{name: "Queue1K_W4", opts: []slogcpasync.Option{slogcpasync.WithQueueSize(1024), slogcpasync.WithWorkerCount(4)}},
		{name: "Queue8K_W8", opts: []slogcpasync.Option{slogcpasync.WithQueueSize(8192), slogcpasync.WithWorkerCount(8)}},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			h := sinkHandler(b, true, tc.opts...)
			benchmarkLogger(b, h)
		})
	}
}
