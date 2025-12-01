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

package slogcpasync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultQueueSize = 1024

	envAsyncEnabled      = "SLOGCP_ASYNC_ENABLED"
	envAsyncQueueSize    = "SLOGCP_ASYNC_QUEUE_SIZE"
	envAsyncDropMode     = "SLOGCP_ASYNC_DROP_MODE"
	envAsyncWorkers      = "SLOGCP_ASYNC_WORKERS"
	envAsyncFlushTimeout = "SLOGCP_ASYNC_FLUSH_TIMEOUT"
)

// DropMode controls how the handler behaves when the queue is full.
type DropMode int

const (
	// DropModeBlock blocks the caller when the queue is full.
	DropModeBlock DropMode = iota
	// DropModeDropNewest drops the incoming record when the queue is full.
	DropModeDropNewest
	// DropModeDropOldest drops the oldest queued record when the queue is full.
	DropModeDropOldest
)

// ErrFlushTimeout indicates Close returned before the queue was fully drained.
var ErrFlushTimeout = errors.New("slogcpasync: flush timeout")

// DropHandler observes dropped records.
type DropHandler func(ctx context.Context, rec slog.Record)

// Config controls async handler behaviour.
type Config struct {
	Enabled      bool
	QueueSize    int
	WorkerCount  int
	BatchSize    int
	DropMode     DropMode
	OnDrop       DropHandler
	ErrorWriter  io.Writer
	FlushTimeout time.Duration

	workerStarter func(func())
}

// Option customizes async handler configuration.
type Option func(*Config)

// WithEnabled toggles the async wrapper on or off.
func WithEnabled(enabled bool) Option {
	return func(cfg *Config) {
		cfg.Enabled = enabled
	}
}

// WithQueueSize adjusts the queue capacity. Zero yields an unbuffered queue.
func WithQueueSize(size int) Option {
	return func(cfg *Config) {
		cfg.QueueSize = size
	}
}

// WithWorkerCount configures the number of worker goroutines.
func WithWorkerCount(count int) Option {
	return func(cfg *Config) {
		cfg.WorkerCount = count
	}
}

// WithBatchSize sets how many queued records a worker drains per wake-up.
// Values less than 1 default to 1.
func WithBatchSize(size int) Option {
	return func(cfg *Config) {
		cfg.BatchSize = size
	}
}

// WithDropMode sets the queue overflow strategy.
func WithDropMode(mode DropMode) Option {
	return func(cfg *Config) {
		cfg.DropMode = mode
	}
}

// WithOnDrop registers a callback invoked when a record is dropped.
func WithOnDrop(fn DropHandler) Option {
	return func(cfg *Config) {
		cfg.OnDrop = fn
	}
}

// WithErrorWriter directs worker errors and panic reports to w. Use nil to
// silence error reporting.
func WithErrorWriter(w io.Writer) Option {
	return func(cfg *Config) {
		cfg.ErrorWriter = w
	}
}

// WithFlushTimeout limits how long Close waits for workers to finish.
func WithFlushTimeout(timeout time.Duration) Option {
	return func(cfg *Config) {
		cfg.FlushTimeout = timeout
	}
}

// WithEnv overlays configuration from environment variables.
func WithEnv() Option {
	return func(cfg *Config) {
		applyEnv(cfg)
	}
}

// Middleware wraps an existing slog.Handler with async behaviour.
func Middleware(opts ...Option) func(slog.Handler) slog.Handler {
	return func(inner slog.Handler) slog.Handler {
		return Wrap(inner, opts...)
	}
}

// Handler is an async slog.Handler wrapper.
type Handler struct {
	inner    slog.Handler
	dropMode DropMode
	onDrop   DropHandler
	state    *asyncState
}

type asyncState struct {
	queue        chan queuedRecord
	wg           sync.WaitGroup
	closed       atomic.Bool
	flushTimeout time.Duration
	closeOnce    sync.Once
	closeErr     error
	closer       func() error
	errWriter    io.Writer
}

type queuedRecord struct {
	ctx     context.Context
	rec     slog.Record
	handler slog.Handler
}

// Wrap returns an async handler around inner unless disabled.
func Wrap(inner slog.Handler, opts ...Option) slog.Handler {
	cfg := buildConfig(opts)
	if !cfg.Enabled {
		return inner
	}
	return newHandler(inner, cfg)
}

// newHandler constructs a Handler and spins up workers according to cfg.
func newHandler(inner slog.Handler, cfg Config) *Handler {
	state := &asyncState{
		queue:        make(chan queuedRecord, cfg.QueueSize),
		flushTimeout: cfg.FlushTimeout,
		closer:       closerFor(inner),
		errWriter:    cfg.ErrorWriter,
	}

	start := func() {
		workerCount := cfg.WorkerCount
		batchSize := cfg.BatchSize
		state.wg.Add(workerCount)
		logError := func(format string, args ...any) {
			if state.errWriter == nil {
				return
			}
			_, _ = fmt.Fprintf(state.errWriter, format, args...)
		}
		handle := func(item queuedRecord) {
			defer func() {
				if r := recover(); r != nil {
					logError("slogcpasync: recovered panic from handler: %v\n", r)
				}
			}()

			if err := item.handler.Handle(item.ctx, item.rec); err != nil {
				logError("slogcpasync: handler error: %v\n", err)
			}
		}

		for range workerCount {
			go func() {
				defer state.wg.Done()
				for item := range state.queue {
					handle(item)
					for n := 1; n < batchSize; n++ {
						select {
						case next, ok := <-state.queue:
							if !ok {
								return
							}
							handle(next)
						default:
							goto nextItem
						}
					}
				nextItem:
				}
			}()
		}
	}

	if cfg.workerStarter != nil {
		cfg.workerStarter(start)
	} else {
		start()
	}

	return &Handler{
		inner:    inner,
		dropMode: cfg.DropMode,
		onDrop:   cfg.OnDrop,
		state:    state,
	}
}

// Enabled defers to the inner handler.
func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

// Handle enqueues a record for async processing.
func (h *Handler) Handle(ctx context.Context, rec slog.Record) error {
	if h.state.closed.Load() {
		if h.onDrop != nil {
			h.onDrop(ctx, rec.Clone())
		}
		return nil
	}

	return h.enqueue(queuedRecord{
		ctx:     ctx,
		rec:     rec.Clone(),
		handler: h.inner,
	})
}

// WithAttrs returns a child handler sharing the same async queue.
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &Handler{
		inner:    h.inner.WithAttrs(attrs),
		dropMode: h.dropMode,
		onDrop:   h.onDrop,
		state:    h.state,
	}
}

// WithGroup returns a child handler sharing the same async queue.
func (h *Handler) WithGroup(name string) slog.Handler {
	return &Handler{
		inner:    h.inner.WithGroup(name),
		dropMode: h.dropMode,
		onDrop:   h.onDrop,
		state:    h.state,
	}
}

// enqueue routes a record into the queue respecting drop policies and recovers from closed channels.
func (h *Handler) enqueue(item queuedRecord) (err error) {
	defer func() {
		if recover() != nil {
			if h.onDrop != nil {
				h.onDrop(item.ctx, item.rec)
			}
			err = nil
		}
	}()

	queue := h.state.queue
	onDrop := h.onDrop

	switch h.dropMode {
	case DropModeDropNewest:
		select {
		case queue <- item:
		default:
			if onDrop != nil {
				onDrop(item.ctx, item.rec)
			}
		}
	case DropModeDropOldest:
		select {
		case queue <- item:
		default:
			var dropped queuedRecord
			select {
			case dropped = <-queue:
			default:
			}
			if onDrop != nil && dropped.handler != nil {
				onDrop(dropped.ctx, dropped.rec)
			}
			select {
			case queue <- item:
			default:
				if onDrop != nil {
					onDrop(item.ctx, item.rec)
				}
			}
		}
	default:
		queue <- item
	}
	return nil
}

// Close flushes the queue then closes the inner handler if it exposes Close.
func (h *Handler) Close() error {
	if h.state == nil {
		return nil
	}

	h.state.closeOnce.Do(func() {
		if h.state.closed.CompareAndSwap(false, true) {
			close(h.state.queue)
		}

		done := make(chan struct{})
		go func() {
			h.state.wg.Wait()
			close(done)
		}()

		if h.state.flushTimeout > 0 {
			select {
			case <-done:
			case <-time.After(h.state.flushTimeout):
				h.state.closeErr = ErrFlushTimeout
			}
		} else {
			<-done
		}

		if h.state.closer != nil {
			if err := h.state.closer(); err != nil && h.state.closeErr == nil {
				h.state.closeErr = err
			}
		}
	})

	return h.state.closeErr
}

// closerFor extracts a Close function from inner when available.
func closerFor(inner slog.Handler) func() error {
	type errorCloser interface {
		Close() error
	}

	if c, ok := inner.(errorCloser); ok {
		return c.Close
	}
	if c, ok := inner.(interface{ Close() }); ok {
		return func() error {
			c.Close()
			return nil
		}
	}
	return nil
}

// buildConfig applies options with defaults and clamps invalid values.
func buildConfig(opts []Option) Config {
	cfg := Config{
		Enabled:     true,
		QueueSize:   defaultQueueSize,
		WorkerCount: 1,
		BatchSize:   1,
		DropMode:    DropModeBlock,
		ErrorWriter: os.Stderr,
	}

	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}

	if cfg.QueueSize < 0 {
		cfg.QueueSize = defaultQueueSize
	}
	if cfg.WorkerCount < 1 {
		cfg.WorkerCount = 1
	}
	if cfg.BatchSize < 1 {
		cfg.BatchSize = 1
	}

	return cfg
}

// applyEnv overlays configuration from environment variables.
func applyEnv(cfg *Config) {
	if raw := strings.TrimSpace(os.Getenv(envAsyncEnabled)); raw != "" {
		if enabled, ok := parseAsyncBool(raw); ok {
			cfg.Enabled = enabled
		}
	}

	if raw := strings.TrimSpace(os.Getenv(envAsyncQueueSize)); raw != "" {
		if size, err := strconv.Atoi(raw); err == nil {
			cfg.QueueSize = size
		}
	}

	if raw := strings.TrimSpace(os.Getenv(envAsyncWorkers)); raw != "" {
		if workers, err := strconv.Atoi(raw); err == nil {
			cfg.WorkerCount = workers
		}
	}

	if raw := strings.TrimSpace(os.Getenv(envAsyncDropMode)); raw != "" {
		switch strings.ToLower(raw) {
		case "block":
			cfg.DropMode = DropModeBlock
		case "drop_newest", "drop-newest":
			cfg.DropMode = DropModeDropNewest
		case "drop_oldest", "drop-oldest":
			cfg.DropMode = DropModeDropOldest
		}
	}

	if raw := strings.TrimSpace(os.Getenv(envAsyncFlushTimeout)); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil {
			cfg.FlushTimeout = d
		}
	}
}

// parseAsyncBool mirrors slogcp's boolean env parsing for yes/on/1/true and no/off/0/false tokens.
func parseAsyncBool(raw string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "t", "true", "yes", "on":
		return true, true
	case "0", "f", "false", "no", "off":
		return false, true
	default:
		return false, false
	}
}
