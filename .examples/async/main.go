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

// Command async demonstrates slogcp's opt-in async wrapper. It wires the
// slogcpasync middleware to the base handler, captures dropped records with
// an OnDrop callback, and keeps Close bounded with a flush timeout.
package main

import (
	"context"
	"io"
	"log"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pjscruggs/slogcp"
	"github.com/pjscruggs/slogcp/slogcpasync"
)

// dropTracker records dropped messages for visibility.
type dropTracker struct {
	dropped  atomic.Int64
	mu       sync.Mutex
	messages []string
}

// newDropTracker constructs a dropTracker that counts dropped records.
func newDropTracker() *dropTracker {
	return &dropTracker{
		messages: []string{},
	}
}

// observe records a dropped slog.Record for later assertions.
func (d *dropTracker) observe(_ context.Context, rec slog.Record) {
	d.dropped.Add(1)

	msg := rec.Message
	if msg == "" {
		return
	}

	d.mu.Lock()
	d.messages = append(d.messages, msg)
	d.mu.Unlock()
}

// count returns the number of observed drops.
func (d *dropTracker) count() int {
	return int(d.dropped.Load())
}

// messagesSnapshot returns a copy of the recorded drop messages.
func (d *dropTracker) messagesSnapshot() []string {
	d.mu.Lock()
	defer d.mu.Unlock()

	out := make([]string, len(d.messages))
	copy(out, d.messages)
	return out
}

// asyncExample builds an async-enabled slogcp logger and tracks drops.
type asyncExample struct {
	handler *slogcp.Handler
	logger  *slog.Logger
	drops   *dropTracker
}

// newAsyncExample constructs a slogcp handler wrapped with slogcpasync using a
// small, but configurable, queue. Environment variables (SLOGCP_ASYNC_*) can
// override the defaults thanks to WithEnv.
func newAsyncExample(writer io.Writer, opts ...slogcpasync.Option) (*asyncExample, error) {
	drops := newDropTracker()

	middlewareOpts := []slogcpasync.Option{
		slogcpasync.WithQueueSize(8),
		slogcpasync.WithWorkerCount(2),
		slogcpasync.WithDropMode(slogcpasync.DropModeDropOldest),
		slogcpasync.WithFlushTimeout(2 * time.Second),
		slogcpasync.WithOnDrop(drops.observe),
		slogcpasync.WithEnv(),
	}
	middlewareOpts = append(middlewareOpts, opts...)

	handler, err := slogcp.NewHandler(writer,
		slogcp.WithMiddleware(
			slogcpasync.Middleware(middlewareOpts...),
		),
	)
	if err != nil {
		return nil, err
	}

	return &asyncExample{
		handler: handler,
		logger:  slog.New(handler),
		drops:   drops,
	}, nil
}

// Close flushes the async handler and any wrapped closers.
func (a *asyncExample) Close() error {
	var firstErr error

	if closer, ok := a.handler.Handler.(interface{ Close() error }); ok {
		if err := closer.Close(); err != nil {
			firstErr = err
		}
	}

	if err := a.handler.Close(); err != nil && firstErr == nil {
		firstErr = err
	}

	return firstErr
}

// logBurst emits a pair of log entries to demonstrate async logging.
func (a *asyncExample) logBurst(ctx context.Context) {
	a.logger.InfoContext(ctx, "accepted background tasks",
		slog.String("component", "scheduler"),
		slog.Int("queued", 3),
	)
	a.logger.WarnContext(ctx, "workers draining async queue",
		slog.String("component", "worker-pool"),
		slog.Int("workers", 2),
	)
}

// main runs the async example and reports dropped log records.
func main() {
	example, err := newAsyncExample(os.Stdout)
	if err != nil {
		log.Fatalf("failed to construct slogcp async example: %v", err)
	}
	defer func() {
		if cerr := example.Close(); cerr != nil {
			log.Printf("async handler close: %v", cerr)
		}
	}()

	ctx := context.Background()
	example.logBurst(ctx)

	if dropped := example.drops.count(); dropped > 0 {
		log.Printf("dropped %d log entries while queue was full", dropped)
	}
}
