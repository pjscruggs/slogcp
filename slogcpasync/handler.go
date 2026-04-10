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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime/debug"
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
	// DropModeDropOldest best-effort drops a queued record to make room for the
	// incoming record when the queue is full. Under contention, eviction and
	// re-enqueue are not atomic, so this mode may still drop the incoming record.
	DropModeDropOldest
)

// ErrFlushTimeout indicates Close returned before the queue was fully drained.
var ErrFlushTimeout = errors.New("slogcpasync: flush timeout")

// ErrAborted indicates shutdown escalated to Abort and records may be dropped.
var ErrAborted = errors.New("slogcpasync: aborted")

// DropHandler observes dropped records.
// Callbacks may run concurrently and should remain fast/non-blocking.
type DropHandler func(ctx context.Context, rec slog.Record)

// Config controls async handler behavior.
type Config struct {
	Enabled       bool
	QueueSize     int
	WorkerCount   int
	BatchSize     int
	DropMode      DropMode
	DetachContext bool
	OnDrop        DropHandler
	ErrorWriter   io.Writer
	FlushTimeout  time.Duration

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
//
// DropModeDropOldest is best-effort under concurrency: it attempts to evict a
// queued record before enqueuing the incoming record, but competing goroutines
// may refill the queue between those steps.
func WithDropMode(mode DropMode) Option {
	return func(cfg *Config) {
		cfg.DropMode = mode
	}
}

// WithDetachedContext controls whether queued records keep the caller's context.
// When enabled, queued records use context.Background(), which avoids retaining
// request-scoped context graphs while records sit in the queue. The tradeoff is
// that downstream handlers cannot read request-scoped context values (for
// example trace/span IDs) during async processing.
func WithDetachedContext(detach bool) Option {
	return func(cfg *Config) {
		cfg.DetachContext = detach
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

// Middleware wraps an existing slog.Handler with async behavior.
func Middleware(opts ...Option) func(slog.Handler) slog.Handler {
	return func(inner slog.Handler) slog.Handler {
		return Wrap(inner, opts...)
	}
}

// Handler is an async slog.Handler wrapper.
type Handler struct {
	inner         slog.Handler
	dropMode      DropMode
	detachContext bool
	onDrop        DropHandler
	state         *asyncState
}

type asyncState struct {
	queue        chan queuedRecord
	wg           sync.WaitGroup
	workersDone  chan struct{}
	closed       atomic.Bool
	aborted      atomic.Bool
	gateMu       sync.Mutex
	gateCond     *sync.Cond
	activeSends  int
	closingCh    chan struct{}
	flushTimeout time.Duration
	closeOnce    sync.Once
	closeErr     error
	shutdownOnce sync.Once
	finalizeOnce sync.Once
	finalizeErr  error
	abortOnce    sync.Once
	abortErr     error
	// abortStartOnce guarantees at most one background abort coordinator starts.
	// This avoids per-call helper goroutine accumulation when callers repeatedly
	// invoke AbortContext with short deadlines.
	abortStartOnce sync.Once
	// abortDone closes once the single abort coordinator finishes.
	abortDone chan struct{}
	// abortResult stores the final joined abort outcome once abortDone closes.
	abortResult error
	closer      func() error
	aborter     func() error
	errWriter   io.Writer
	onDrop      DropHandler
}

type queuedRecord struct {
	// ctx is carried so downstream handlers can read context values. Per the
	// slog.Handler contract, cancellation/deadlines do not suppress processing.
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
		inner:         inner,
		dropMode:      cfg.DropMode,
		detachContext: cfg.DetachContext,
		onDrop:        cfg.OnDrop,
		state:         state,
	}
}

// newAsyncState initializes async queue state for the wrapper.
func newAsyncState(inner slog.Handler, cfg Config) *asyncState {
	state := &asyncState{
		queue:        make(chan queuedRecord, cfg.QueueSize),
		closingCh:    make(chan struct{}),
		flushTimeout: cfg.FlushTimeout,
		closer:       closerFor(inner),
		aborter:      aborterFor(inner),
		errWriter:    cfg.ErrorWriter,
		onDrop:       cfg.OnDrop,
	}
	state.gateCond = sync.NewCond(&state.gateMu)
	return state
}

// startAsyncWorkers launches worker goroutines via the optional starter hook.
func startAsyncWorkers(state *asyncState, cfg Config) {
	start := func() {
		state.wg.Add(cfg.WorkerCount)
		for range cfg.WorkerCount {
			go state.worker(cfg.BatchSize)
		}
		state.startWorkersDoneMonitor()
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
		s.processItem(item)
		s.drainBatch(batchSize)
	}
}

// startWorkersDoneMonitor closes workersDone after all workers exit.
func (s *asyncState) startWorkersDoneMonitor() {
	if s == nil {
		return
	}

	s.gateMu.Lock()
	if s.workersDone != nil {
		s.gateMu.Unlock()
		return
	}
	done := make(chan struct{})
	s.workersDone = done
	s.gateMu.Unlock()

	go func() {
		s.wg.Wait()
		close(done)
	}()
}

// processItem forwards or drops an item depending on abort state.
func (s *asyncState) processItem(item queuedRecord) {
	if s.aborted.Load() {
		s.reportDrop(item)
		return
	}
	s.handleItem(item)
}

// handleItem safely invokes the inner handler, logging panics and errors.
func (s *asyncState) handleItem(item queuedRecord) {
	defer func() {
		if r := recover(); r != nil {
			s.reportDrop(item)
			s.logError("slogcpasync: recovered panic from handler: %v\n%s", r, debug.Stack())
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
			s.processItem(next)
		default:
			return
		}
	}
}

// reportDrop invokes onDrop for dropped records when configured.
func (s *asyncState) reportDrop(item queuedRecord) {
	if s == nil || s.onDrop == nil || item.handler == nil {
		return
	}
	s.onDrop(item.ctx, item.rec)
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
// Context cancellation does not suppress emission: ctx is treated as a value
// carrier for handlers, matching slog.Handler semantics.
func (h *Handler) Handle(ctx context.Context, rec slog.Record) error {
	rec = rec.Clone()
	queuedCtx := ctx
	if h.detachContext {
		queuedCtx = context.Background()
	}

	if !h.state.beginSend() {
		if h.onDrop != nil {
			h.onDrop(ctx, rec)
		}
		return nil
	}
	defer h.state.endSend()

	return h.enqueue(queuedRecord{
		ctx:     queuedCtx,
		rec:     rec,
		handler: h.inner,
	})
}

// WithAttrs returns a child handler sharing the same async queue.
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &Handler{
		inner:         h.inner.WithAttrs(attrs),
		dropMode:      h.dropMode,
		detachContext: h.detachContext,
		onDrop:        h.onDrop,
		state:         h.state,
	}
}

// WithGroup returns a child handler sharing the same async queue.
func (h *Handler) WithGroup(name string) slog.Handler {
	return &Handler{
		inner:         h.inner.WithGroup(name),
		dropMode:      h.dropMode,
		detachContext: h.detachContext,
		onDrop:        h.onDrop,
		state:         h.state,
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
		h.enqueueBlock(item)
	}
	return nil
}

// enqueueBlock blocks until the queue accepts the item or shutdown starts.
func (h *Handler) enqueueBlock(item queuedRecord) {
	select {
	case <-h.state.closingCh:
		h.drop(item)
		return
	default:
	}

	select {
	case h.state.queue <- item:
	case <-h.state.closingCh:
		h.drop(item)
	}
}

// enqueueDropNewest drops the incoming item when the queue is full.
func (h *Handler) enqueueDropNewest(item queuedRecord) {
	select {
	case <-h.state.closingCh:
		h.drop(item)
		return
	default:
	}

	select {
	case h.state.queue <- item:
	default:
		h.drop(item)
	}
}

// enqueueDropOldest evicts the oldest queued record before enqueuing item.
func (h *Handler) enqueueDropOldest(item queuedRecord) {
	select {
	case <-h.state.closingCh:
		h.drop(item)
		return
	default:
	}

	select {
	case h.state.queue <- item:
		return
	default:
		// This two-step path is intentionally non-atomic. Under contention another
		// sender can win the slot between eviction and retry, so DropOldest is
		// best-effort and may degrade to dropping the incoming item.
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

// Close implements io.Closer by delegating to Shutdown with the configured
// flush timeout. A timeout returns ErrFlushTimeout (joined with the context
// deadline error) and leaves in-flight worker calls running until callers
// explicitly Abort.
func (h *Handler) Close() error {
	if h.state == nil {
		return nil
	}

	h.state.closeOnce.Do(func() {
		ctx := context.Background()
		cancel := func() {}
		if h.state.flushTimeout > 0 {
			ctx, cancel = context.WithTimeout(context.Background(), h.state.flushTimeout)
		}
		defer cancel()
		h.state.closeErr = h.Shutdown(ctx)
	})

	return h.state.closeErr
}

// lifecycleContext preserves the async handler's defensive nil-context
// behavior for shutdown paths by treating nil the same as context.Background().
func lifecycleContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// Shutdown begins graceful shutdown, waits for workers until ctx expires,
// then closes the wrapped handler (if closable) after workers exit. A nil ctx
// is treated as context.Background().
func (h *Handler) Shutdown(ctx context.Context) error {
	if h == nil || h.state == nil {
		return nil
	}

	h.state.startShutdown()

	if err := h.state.waitWorkers(lifecycleContext(ctx)); err != nil {
		return errors.Join(ErrFlushTimeout, err)
	}

	closeErr := h.state.finalizeClose()
	if h.state.aborted.Load() {
		return errors.Join(ErrAborted, closeErr)
	}
	return closeErr
}

// Abort escalates shutdown to best-effort termination, dropping queued records
// not yet handled. Abort blocks until the abort sequence completes.
//
// For bounded waits, use [AbortContext].
func (h *Handler) Abort() error {
	return h.AbortContext(context.Background())
}

// AbortContext initiates the forceful abort sequence and waits until completion
// or ctx expiry. A nil ctx is treated as context.Background().
//
// Cancellation only bounds the caller's wait; abort continues in the background
// once started. This method intentionally starts at most one abort coordinator
// goroutine and reuses its completion signal across calls so repeated short
// timeouts do not accumulate helper goroutines.
func (h *Handler) AbortContext(ctx context.Context) error {
	if h == nil || h.state == nil {
		return nil
	}
	ctx = lifecycleContext(ctx)

	done := h.state.startAbort()
	select {
	case <-done:
		return h.state.abortOutcome()
	case <-ctx.Done():
		return errors.Join(ErrAborted, ctx.Err())
	}
}

// beginSend tracks an in-flight send while the handler remains open.
func (s *asyncState) beginSend() bool {
	if s == nil {
		return false
	}

	s.gateMu.Lock()
	defer s.gateMu.Unlock()
	s.ensureGateLocked()
	if s.closed.Load() {
		return false
	}
	s.activeSends++
	return true
}

// endSend marks an in-flight send as complete.
func (s *asyncState) endSend() {
	if s == nil {
		return
	}

	s.gateMu.Lock()
	if s.activeSends > 0 {
		s.activeSends--
		if s.activeSends == 0 && s.gateCond != nil {
			s.gateCond.Broadcast()
		}
	}
	s.gateMu.Unlock()
}

// beginClose prevents new sends, unblocks blocked senders, and waits for active senders to finish.
func (s *asyncState) beginClose() {
	s.gateMu.Lock()
	defer s.gateMu.Unlock()

	s.ensureGateLocked()

	select {
	case <-s.closingCh:
	default:
		close(s.closingCh)
	}

	for s.activeSends > 0 {
		s.gateCond.Wait()
	}
}

// endClose closes the queue once all active senders have exited.
func (s *asyncState) endClose() {
	if s == nil || s.queue == nil {
		return
	}
	defer func() {
		_ = recover()
	}()
	close(s.queue)
}

// startShutdown performs the one-time transition to the closing state.
func (s *asyncState) startShutdown() {
	if s == nil {
		return
	}
	s.shutdownOnce.Do(func() {
		s.closed.Store(true)
		s.beginClose()
		s.endClose()
	})
}

// waitWorkers waits for all workers or ctx cancellation/deadline.
func (s *asyncState) waitWorkers(ctx context.Context) error {
	if s == nil {
		return nil
	}

	done := s.workersDoneChannel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wait workers: %w", ctx.Err())
	}
}

// workersDoneChannel returns a channel that closes once workers exit.
func (s *asyncState) workersDoneChannel() <-chan struct{} {
	s.gateMu.Lock()
	done := s.workersDone
	s.gateMu.Unlock()
	if done != nil {
		return done
	}

	// When workers were never started (for example in tests), there is nothing
	// to wait for.
	ch := make(chan struct{})
	close(ch)
	return ch
}

// finalizeClose closes the wrapped handler exactly once.
func (s *asyncState) finalizeClose() error {
	if s == nil {
		return nil
	}
	s.finalizeOnce.Do(func() {
		if s.closer != nil {
			s.finalizeErr = s.closer()
		}
	})
	return s.finalizeErr
}

// abortDownstream invokes an optional downstream abort hook once.
func (s *asyncState) abortDownstream() error {
	if s == nil {
		return nil
	}
	s.abortOnce.Do(func() {
		if s.aborter != nil {
			s.abortErr = s.aborter()
		}
	})
	return s.abortErr
}

// startAbort transitions to abort mode once and runs abort/close orchestration
// in a single background coordinator.
func (s *asyncState) startAbort() <-chan struct{} {
	if s == nil {
		done := make(chan struct{})
		close(done)
		return done
	}

	s.abortStartOnce.Do(func() {
		s.abortDone = make(chan struct{})
		s.aborted.Store(true)
		s.startShutdown()

		go func() {
			abortErr := s.abortDownstream()
			waitErr := s.waitWorkers(context.Background())
			closeErr := s.finalizeClose()
			s.abortResult = errors.Join(ErrAborted, abortErr, waitErr, closeErr)
			close(s.abortDone)
		}()
	})

	if s.abortDone == nil {
		done := make(chan struct{})
		close(done)
		return done
	}
	return s.abortDone
}

// abortOutcome returns the final abort result after startAbort completion.
func (s *asyncState) abortOutcome() error {
	if s == nil {
		return nil
	}
	if s.abortResult != nil {
		return s.abortResult
	}
	return ErrAborted
}

// ensureGateLocked initializes coordination primitives for tests that build asyncState directly.
func (s *asyncState) ensureGateLocked() {
	if s.gateCond == nil {
		s.gateCond = sync.NewCond(&s.gateMu)
	}
	if s.closingCh == nil {
		s.closingCh = make(chan struct{})
	}
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

// aborterFor extracts an explicit abort hook from inner when available.
func aborterFor(inner slog.Handler) func() error {
	type errorAborter interface {
		Abort() error
	}

	if a, ok := inner.(errorAborter); ok {
		return a.Abort
	}
	if a, ok := inner.(interface{ Abort() }); ok {
		return func() error {
			a.Abort()
			return nil
		}
	}
	return nil
}

// buildConfig applies options with defaults and clamps invalid values.
func buildConfig(opts []Option) Config {
	cfg := defaultAsyncConfig()
	applyConfigOptions(&cfg, opts)

	defaults := resolveModeDefaults(cfg.DropMode)
	cfg.QueueSize = resolveQueueSize(cfg.QueueSize, cfg.queueSet, defaults.queue)
	cfg.WorkerCount = clampMinimum(cfg.WorkerCount, cfg.workerCountSet, defaults.workers, 1)
	cfg.BatchSize = clampMinimum(cfg.BatchSize, cfg.batchSizeSet, defaults.batch, 1)
	return cfg
}

// defaultAsyncConfig returns the baseline async configuration.
func defaultAsyncConfig() Config {
	return Config{
		Enabled:     true,
		DropMode:    DropModeBlock,
		ErrorWriter: os.Stderr,
	}
}

// applyConfigOptions applies provided options to the config.
func applyConfigOptions(cfg *Config, opts []Option) {
	for _, opt := range opts {
		if opt != nil {
			opt(cfg)
		}
	}
}

// resolveModeDefaults returns the defaults for the selected drop mode.
func resolveModeDefaults(mode DropMode) modeDefault {
	if defaults := modeDefaults[mode]; defaults != (modeDefault{}) {
		return defaults
	}
	return modeDefaults[DropModeBlock]
}

// resolveQueueSize clamps queue sizing using defaults when unset or invalid.
func resolveQueueSize(queue int, queueSet bool, fallback int) int {
	if !queueSet || queue < 0 {
		return fallback
	}
	return queue
}

// clampMinimum clamps integers to a minimum unless explicitly set higher.
func clampMinimum(value int, explicitlySet bool, fallback int, minValue int) int {
	if !explicitlySet || value < minValue {
		return fallback
	}
	return value
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
	if enabled, err := strconv.ParseBool(raw); err == nil {
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
