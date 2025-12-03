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
	defaultQueueSizeBlock      = 2048
	defaultQueueSizeDropNewest = 512
	defaultQueueSizeDropOldest = 1024

	defaultWorkerCountBlock      = 1
	defaultWorkerCountDropNewest = 1
	defaultWorkerCountDropOldest = 1

	defaultBatchSizeBlock      = 1
	defaultBatchSizeDropNewest = 1
	defaultBatchSizeDropOldest = 1

	// Retain the block-mode queue size for backwards-compatible tests.
	defaultQueueSize = defaultQueueSizeBlock

	envAsyncEnabled      = "SLOGCP_ASYNC"
	envAsyncQueueSize    = "SLOGCP_ASYNC_QUEUE_SIZE"
	envAsyncDropMode     = "SLOGCP_ASYNC_DROP_MODE"
	envAsyncWorkers      = "SLOGCP_ASYNC_WORKERS"
	envAsyncFlushTimeout = "SLOGCP_ASYNC_FLUSH_TIMEOUT"
)

type modeDefault struct {
	queue   int
	workers int
	batch   int
}

var modeDefaults = map[DropMode]modeDefault{
	DropModeBlock: {
		queue:   defaultQueueSizeBlock,
		workers: defaultWorkerCountBlock,
		batch:   defaultBatchSizeBlock,
	},
	DropModeDropNewest: {
		queue:   defaultQueueSizeDropNewest,
		workers: defaultWorkerCountDropNewest,
		batch:   defaultBatchSizeDropNewest,
	},
	DropModeDropOldest: {
		queue:   defaultQueueSizeDropOldest,
		workers: defaultWorkerCountDropOldest,
		batch:   defaultBatchSizeDropOldest,
	},
}

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

	queueSet       bool
	workerCountSet bool
	batchSizeSet   bool
	workerStarter  func(func())
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
		cfg.queueSet = true
	}
}

// WithWorkerCount configures the number of worker goroutines.
func WithWorkerCount(count int) Option {
	return func(cfg *Config) {
		cfg.WorkerCount = count
		cfg.workerCountSet = true
	}
}

// WithBatchSize sets how many queued records a worker drains per wake-up.
// Values less than 1 fall back to the drop-mode baseline.
func WithBatchSize(size int) Option {
	return func(cfg *Config) {
		cfg.BatchSize = size
		cfg.batchSizeSet = true
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
	state := newAsyncState(inner, cfg)
	startAsyncWorkers(state, cfg)

	return &Handler{
		inner:    inner,
		dropMode: cfg.DropMode,
		onDrop:   cfg.OnDrop,
		state:    state,
	}
}

// newAsyncState initializes async queue state for the wrapper.
func newAsyncState(inner slog.Handler, cfg Config) *asyncState {
	return &asyncState{
		queue:        make(chan queuedRecord, cfg.QueueSize),
		flushTimeout: cfg.FlushTimeout,
		closer:       closerFor(inner),
		errWriter:    cfg.ErrorWriter,
	}
}

// startAsyncWorkers launches worker goroutines via the optional starter hook.
func startAsyncWorkers(state *asyncState, cfg Config) {
	start := func() {
		state.wg.Add(cfg.WorkerCount)
		for range cfg.WorkerCount {
			go state.worker(cfg.BatchSize)
		}
	}

	if cfg.workerStarter != nil {
		cfg.workerStarter(start)
		return
	}
	start()
}

// worker drains the queue, handling items in batches.
func (s *asyncState) worker(batchSize int) {
	defer s.wg.Done()
	for item := range s.queue {
		s.handleItem(item)
		s.drainBatch(batchSize)
	}
}

// handleItem safely invokes the inner handler, logging panics and errors.
func (s *asyncState) handleItem(item queuedRecord) {
	defer func() {
		if r := recover(); r != nil {
			s.logError("slogcpasync: recovered panic from handler: %v\n", r)
		}
	}()

	if err := item.handler.Handle(item.ctx, item.rec); err != nil {
		s.logError("slogcpasync: handler error: %v\n", err)
	}
}

// drainBatch processes up to batchSize-1 additional queued items.
func (s *asyncState) drainBatch(batchSize int) {
	for n := 1; n < batchSize; n++ {
		select {
		case next, ok := <-s.queue:
			if !ok {
				return
			}
			s.handleItem(next)
		default:
			return
		}
	}
}

// logError writes formatted errors to the configured error writer.
func (s *asyncState) logError(format string, args ...any) {
	if s.errWriter == nil {
		return
	}
	_, _ = fmt.Fprintf(s.errWriter, format, args...)
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

	switch h.dropMode {
	case DropModeDropNewest:
		h.enqueueDropNewest(item)
	case DropModeDropOldest:
		h.enqueueDropOldest(item)
	default:
		h.state.queue <- item
	}
	return nil
}

// enqueueDropNewest drops the incoming item when the queue is full.
func (h *Handler) enqueueDropNewest(item queuedRecord) {
	select {
	case h.state.queue <- item:
	default:
		h.drop(item)
	}
}

// enqueueDropOldest evicts the oldest queued record before enqueuing item.
func (h *Handler) enqueueDropOldest(item queuedRecord) {
	select {
	case h.state.queue <- item:
		return
	default:
		h.dropOldest()
		h.enqueueDropNewest(item)
	}
}

// dropOldest removes and reports the oldest queued record when present.
func (h *Handler) dropOldest() {
	select {
	case dropped := <-h.state.queue:
		if h.onDrop != nil && dropped.handler != nil {
			h.onDrop(dropped.ctx, dropped.rec)
		}
	default:
	}
}

// drop invokes the onDrop callback if configured.
func (h *Handler) drop(item queuedRecord) {
	if h.onDrop == nil {
		return
	}
	h.onDrop(item.ctx, item.rec)
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
		DropMode:    DropModeBlock,
		ErrorWriter: os.Stderr,
	}

	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}

	defaults := modeDefaults[cfg.DropMode]
	if defaults == (modeDefault{}) {
		defaults = modeDefaults[DropModeBlock]
	}

	if !cfg.queueSet {
		cfg.QueueSize = defaults.queue
	}
	if cfg.QueueSize < 0 {
		cfg.QueueSize = defaults.queue
	}

	if !cfg.workerCountSet {
		cfg.WorkerCount = defaults.workers
	}
	if cfg.WorkerCount < 1 {
		cfg.WorkerCount = defaults.workers
	}

	if !cfg.batchSizeSet {
		cfg.BatchSize = defaults.batch
	}
	if cfg.BatchSize < 1 {
		cfg.BatchSize = defaults.batch
	}

	return cfg
}

// applyEnv overlays configuration from environment variables.
func applyEnv(cfg *Config) {
	applyAsyncEnabled(cfg)
	applyAsyncQueueSize(cfg)
	applyAsyncWorkers(cfg)
	applyAsyncDropMode(cfg)
	applyAsyncFlushTimeout(cfg)
}

// applyAsyncEnabled reads async enablement from env.
func applyAsyncEnabled(cfg *Config) {
	raw := strings.TrimSpace(os.Getenv(envAsyncEnabled))
	if raw == "" {
		return
	}
	if enabled, ok := parseAsyncBool(raw); ok {
		cfg.Enabled = enabled
	}
}

// applyAsyncQueueSize parses queue size from env.
func applyAsyncQueueSize(cfg *Config) {
	raw := strings.TrimSpace(os.Getenv(envAsyncQueueSize))
	if raw == "" {
		return
	}
	if size, err := strconv.Atoi(raw); err == nil {
		cfg.QueueSize = size
		cfg.queueSet = true
	}
}

// applyAsyncWorkers parses worker count from env.
func applyAsyncWorkers(cfg *Config) {
	raw := strings.TrimSpace(os.Getenv(envAsyncWorkers))
	if raw == "" {
		return
	}
	if workers, err := strconv.Atoi(raw); err == nil {
		cfg.WorkerCount = workers
		cfg.workerCountSet = true
	}
}

// applyAsyncDropMode reads drop mode overrides from env.
func applyAsyncDropMode(cfg *Config) {
	raw := strings.TrimSpace(os.Getenv(envAsyncDropMode))
	if raw == "" {
		return
	}
	switch strings.ToLower(raw) {
	case "block":
		cfg.DropMode = DropModeBlock
	case "drop_newest", "drop-newest":
		cfg.DropMode = DropModeDropNewest
	case "drop_oldest", "drop-oldest":
		cfg.DropMode = DropModeDropOldest
	}
}

// applyAsyncFlushTimeout parses flush timeout durations from env.
func applyAsyncFlushTimeout(cfg *Config) {
	raw := strings.TrimSpace(os.Getenv(envAsyncFlushTimeout))
	if raw == "" {
		return
	}
	if d, err := time.ParseDuration(raw); err == nil {
		cfg.FlushTimeout = d
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
