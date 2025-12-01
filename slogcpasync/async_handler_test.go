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
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recordingState tracks records handled by a test double handler.
type recordingState struct {
	mu         sync.Mutex
	records    []slog.Record
	contexts   []context.Context
	closeCount int
	calls      chan struct{}
}

// recordingHandler implements slog.Handler for tests.
type recordingHandler struct {
	state  *recordingState
	attrs  []slog.Attr
	groups []string
	block  <-chan struct{}
}

// newRecordingHandler builds a slog.Handler test double that records received
// records and optionally blocks until the supplied channel closes.
func newRecordingHandler(block <-chan struct{}) *recordingHandler {
	return &recordingHandler{
		state: &recordingState{
			calls: make(chan struct{}, 32),
		},
		block: block,
	}
}

type erroringHandler struct {
	*recordingHandler
	err error
}

// Handle forwards to the recorder and returns a configured error for testing.
func (h *erroringHandler) Handle(ctx context.Context, rec slog.Record) error {
	_ = h.recordingHandler.Handle(ctx, rec)
	return h.err
}

type panicOnceHandler struct {
	*recordingHandler
	panicked atomic.Bool
}

// Handle panics once then records subsequent calls.
func (h *panicOnceHandler) Handle(ctx context.Context, rec slog.Record) error {
	if !h.panicked.Swap(true) {
		panic("boom")
	}
	return h.recordingHandler.Handle(ctx, rec)
}

// Enabled always reports records as loggable for testing.
func (h *recordingHandler) Enabled(context.Context, slog.Level) bool {
	return true
}

// Handle clones and records the log entry for assertions.
func (h *recordingHandler) Handle(ctx context.Context, rec slog.Record) error {
	if h.block != nil {
		<-h.block
	}

	rec = rec.Clone()
	if len(h.attrs) > 0 {
		rec.AddAttrs(h.attrs...)
	}

	h.state.mu.Lock()
	h.state.records = append(h.state.records, rec)
	h.state.contexts = append(h.state.contexts, ctx)
	h.state.mu.Unlock()

	select {
	case h.state.calls <- struct{}{}:
	default:
	}

	return nil
}

// WithAttrs returns a child handler that appends attributes.
func (h *recordingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	dup := make([]slog.Attr, len(attrs))
	copy(dup, attrs)
	return &recordingHandler{
		state:  h.state,
		attrs:  append(append([]slog.Attr(nil), h.attrs...), dup...),
		groups: append([]string(nil), h.groups...),
		block:  h.block,
	}
}

// WithGroup returns a child handler with an extra group marker.
func (h *recordingHandler) WithGroup(name string) slog.Handler {
	return &recordingHandler{
		state:  h.state,
		attrs:  append([]slog.Attr(nil), h.attrs...),
		groups: append(append([]string(nil), h.groups...), name),
		block:  h.block,
	}
}

// Close increments a counter for assertions.
func (h *recordingHandler) Close() error {
	h.state.mu.Lock()
	h.state.closeCount++
	h.state.mu.Unlock()
	return nil
}

// waitForCalls blocks until the handler recorded n invocations or times out.
func waitForCalls(t *testing.T, c <-chan struct{}, n int) {
	t.Helper()
	timeout := time.After(500 * time.Millisecond)
	for i := 0; i < n; i++ {
		select {
		case <-c:
		case <-timeout:
			t.Fatalf("timed out waiting for %d handler calls", n)
		}
	}
}

// withWorkerStarter installs a workerStarter hook for tests.
func withWorkerStarter(starter func(func())) Option {
	return func(cfg *Config) {
		cfg.workerStarter = starter
	}
}

// TestWrapNonBlockingAndFlushesOnClose verifies queueing is non-blocking and Close flushes.
func TestWrapNonBlockingAndFlushesOnClose(t *testing.T) {
	t.Parallel()

	block := make(chan struct{})
	inner := newRecordingHandler(block)
	h := Wrap(inner, WithQueueSize(1), WithWorkerCount(1))

	rec := slog.NewRecord(time.Now(), slog.LevelInfo, "hello", 0)

	done := make(chan error, 1)
	go func() {
		done <- h.Handle(context.Background(), rec)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Handle returned %v, want nil", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Handle blocked unexpectedly")
	}

	if len(inner.state.records) != 0 {
		t.Fatalf("inner recorded %d records before unblock, want 0", len(inner.state.records))
	}

	close(block)
	waitForCalls(t, inner.state.calls, 1)

	if err := h.(*Handler).Close(); err != nil {
		t.Fatalf("Handler.Close() returned %v, want nil", err)
	}

	if got := len(inner.state.records); got != 1 {
		t.Fatalf("inner handled %d records, want 1", got)
	}
	if inner.state.records[0].Message != "hello" {
		t.Fatalf("record message = %q, want %q", inner.state.records[0].Message, "hello")
	}
	if inner.state.closeCount != 1 {
		t.Fatalf("inner Close() called %d times, want 1", inner.state.closeCount)
	}
}

// TestDropNewestDropsWhenQueueBusy ensures drop-newest overflow policy fires.
func TestDropNewestDropsWhenQueueBusy(t *testing.T) {
	t.Parallel()

	start := make(chan func(), 1)
	inner := newRecordingHandler(nil)
	var dropped []string

	h := Wrap(inner,
		WithQueueSize(1),
		WithDropMode(DropModeDropNewest),
		WithOnDrop(func(_ context.Context, rec slog.Record) {
			dropped = append(dropped, rec.Message)
		}),
		withWorkerStarter(func(run func()) {
			start <- run
		}),
	)

	first := slog.NewRecord(time.Now(), slog.LevelInfo, "first", 0)
	second := slog.NewRecord(time.Now(), slog.LevelInfo, "second", 0)

	if err := h.Handle(context.Background(), first); err != nil {
		t.Fatalf("Handle(first) returned %v, want nil", err)
	}

	if err := h.Handle(context.Background(), second); err != nil {
		t.Fatalf("Handle(second) returned %v, want nil", err)
	}

	run := <-start
	run()

	waitForCalls(t, inner.state.calls, 1)

	if err := h.(*Handler).Close(); err != nil {
		t.Fatalf("Handler.Close() returned %v, want nil", err)
	}

	if got := len(inner.state.records); got != 1 {
		t.Fatalf("inner handled %d records, want 1", got)
	}
	if inner.state.records[0].Message != "first" {
		t.Fatalf("handled message = %q, want %q", inner.state.records[0].Message, "first")
	}
	if len(dropped) != 1 || dropped[0] != "second" {
		t.Fatalf("dropped = %v, want [second]", dropped)
	}
}

// TestDropOldestEvictsBufferedRecord ensures the oldest buffered record is evicted.
func TestDropOldestEvictsBufferedRecord(t *testing.T) {
	t.Parallel()

	start := make(chan func(), 1)
	inner := newRecordingHandler(nil)

	h := Wrap(inner,
		WithQueueSize(1),
		WithDropMode(DropModeDropOldest),
		WithWorkerCount(1),
		withWorkerStarter(func(run func()) {
			start <- run
		}),
	)

	first := slog.NewRecord(time.Now(), slog.LevelInfo, "first", 0)
	second := slog.NewRecord(time.Now(), slog.LevelInfo, "second", 0)

	if err := h.Handle(context.Background(), first); err != nil {
		t.Fatalf("Handle(first) returned %v, want nil", err)
	}
	if err := h.Handle(context.Background(), second); err != nil {
		t.Fatalf("Handle(second) returned %v, want nil", err)
	}

	run := <-start
	run()

	waitForCalls(t, inner.state.calls, 1)

	if err := h.(*Handler).Close(); err != nil {
		t.Fatalf("Handler.Close() returned %v, want nil", err)
	}

	if got := len(inner.state.records); got != 1 {
		t.Fatalf("inner handled %d records, want 1", got)
	}
	if inner.state.records[0].Message != "second" {
		t.Fatalf("handled message = %q, want %q", inner.state.records[0].Message, "second")
	}
}

// TestDropOldestInvokesOnDropForEvictedRecord reports dropped entries via OnDrop.
func TestDropOldestInvokesOnDropForEvictedRecord(t *testing.T) {
	t.Parallel()

	start := make(chan func(), 1)
	inner := newRecordingHandler(nil)
	var dropped []string

	h := Wrap(inner,
		WithQueueSize(1),
		WithDropMode(DropModeDropOldest),
		WithOnDrop(func(_ context.Context, rec slog.Record) {
			dropped = append(dropped, rec.Message)
		}),
		withWorkerStarter(func(run func()) {
			start <- run
		}),
	)

	first := slog.NewRecord(time.Now(), slog.LevelInfo, "first", 0)
	second := slog.NewRecord(time.Now(), slog.LevelInfo, "second", 0)

	if err := h.Handle(context.Background(), first); err != nil {
		t.Fatalf("Handle(first) returned %v, want nil", err)
	}
	if err := h.Handle(context.Background(), second); err != nil {
		t.Fatalf("Handle(second) returned %v, want nil", err)
	}

	run := <-start
	run()

	waitForCalls(t, inner.state.calls, 1)

	if err := h.(*Handler).Close(); err != nil {
		t.Fatalf("Handler.Close() returned %v, want nil", err)
	}

	if got := len(inner.state.records); got != 1 {
		t.Fatalf("inner handled %d records, want 1", got)
	}
	if inner.state.records[0].Message != "second" {
		t.Fatalf("handled message = %q, want %q", inner.state.records[0].Message, "second")
	}
	if len(dropped) != 1 || dropped[0] != "first" {
		t.Fatalf("dropped = %v, want [first]", dropped)
	}
}

// TestHandleTracksDropsUnderBackpressure counts handled vs dropped when writers overwhelm the queue.
func TestHandleTracksDropsUnderBackpressure(t *testing.T) {
	t.Parallel()

	var dropped atomic.Int64
	block := make(chan struct{})
	inner := newRecordingHandler(block)

	handler := Wrap(inner,
		WithQueueSize(8),
		WithWorkerCount(1),
		WithDropMode(DropModeDropNewest),
		WithOnDrop(func(_ context.Context, rec slog.Record) {
			dropped.Add(1)
			_ = rec // ensure rec.Clone() exercised
		}),
	)

	total := 200
	rec := slog.NewRecord(time.Now(), slog.LevelInfo, "flood", 0)

	for i := 0; i < total; i++ {
		if err := handler.Handle(context.Background(), rec); err != nil {
			t.Fatalf("Handle returned %v", err)
		}
	}

	close(block) // unblock worker so Close can drain what was queued

	if err := handler.(*Handler).Close(); err != nil {
		t.Fatalf("Close returned %v", err)
	}

	handled := len(inner.state.records)
	droppedCount := int(dropped.Load())

	if handled == total {
		t.Fatalf("expected some drops under backpressure, handled all %d", handled)
	}
	if handled+droppedCount != total {
		t.Fatalf("handled+dropped = %d, total = %d", handled+droppedCount, total)
	}
	if droppedCount == 0 {
		t.Fatalf("expected dropped records under backpressure")
	}
}

// TestWorkerLogsErrorsAndContinues reports handler errors to the configured writer.
func TestWorkerLogsErrorsAndContinues(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	inner := &erroringHandler{
		recordingHandler: newRecordingHandler(nil),
		err:              errors.New("boom"),
	}
	h := Wrap(inner, WithErrorWriter(&buf))

	if err := h.Handle(context.Background(), slog.NewRecord(time.Now(), slog.LevelInfo, "msg", 0)); err != nil {
		t.Fatalf("Handle returned %v, want nil", err)
	}
	if err := h.(*Handler).Close(); err != nil {
		t.Fatalf("Close returned %v, want nil", err)
	}

	if got := len(inner.state.records); got != 1 {
		t.Fatalf("handled %d records, want 1", got)
	}
	if out := buf.String(); !strings.Contains(out, "boom") || !strings.Contains(out, "handler error") {
		t.Fatalf("error output = %q, want contains handler error and boom", out)
	}
}

// TestWorkerRecoversFromPanic ensures panics do not kill the worker goroutine.
func TestWorkerRecoversFromPanic(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	inner := &panicOnceHandler{recordingHandler: newRecordingHandler(nil)}
	h := Wrap(inner, WithQueueSize(2), WithErrorWriter(&buf))

	first := slog.NewRecord(time.Now(), slog.LevelInfo, "first", 0)
	second := slog.NewRecord(time.Now(), slog.LevelInfo, "second", 0)

	if err := h.Handle(context.Background(), first); err != nil {
		t.Fatalf("Handle(first) returned %v, want nil", err)
	}
	if err := h.Handle(context.Background(), second); err != nil {
		t.Fatalf("Handle(second) returned %v, want nil", err)
	}

	if err := h.(*Handler).Close(); err != nil {
		t.Fatalf("Close returned %v, want nil", err)
	}

	if got := len(inner.state.records); got != 1 {
		t.Fatalf("handled %d records, want 1 post-panic", got)
	}
	if inner.state.records[0].Message != "second" {
		t.Fatalf("handled message = %q, want %q", inner.state.records[0].Message, "second")
	}
	if out := buf.String(); !strings.Contains(out, "recovered panic") || !strings.Contains(out, "boom") {
		t.Fatalf("panic output = %q, want recovered panic notice", out)
	}
}

// TestWithAttrsAndGroupsShareQueue exercises derived handlers sharing the queue.
func TestWithAttrsAndGroupsShareQueue(t *testing.T) {
	t.Parallel()

	inner := newRecordingHandler(nil)
	handler := Wrap(inner, WithQueueSize(4))

	logger := slog.New(handler)
	child := logger.With("component", "child").WithGroup("http")

	logger.Info("root")
	child.Info("child")

	if err := handler.(*Handler).Close(); err != nil {
		t.Fatalf("Handler.Close() returned %v, want nil", err)
	}

	if got := len(inner.state.records); got != 2 {
		t.Fatalf("inner handled %d records, want 2", got)
	}

	attrs := func(rec slog.Record) map[string]any {
		out := map[string]any{}
		rec.Attrs(func(a slog.Attr) bool {
			out[a.Key] = a.Value.Any()
			return true
		})
		return out
	}

	rootAttrs := attrs(inner.state.records[0])
	if _, ok := rootAttrs["component"]; ok {
		t.Fatalf("root record unexpectedly contained component attribute: %v", rootAttrs)
	}

	childAttrs := attrs(inner.state.records[1])
	if got := childAttrs["component"]; got != "child" {
		t.Fatalf("component attr = %v, want child", got)
	}
}

// TestMiddlewareRespectsEnvToggle disables async via env and ensures middleware is skipped.
func TestMiddlewareRespectsEnvToggle(t *testing.T) {
	t.Setenv(envAsyncEnabled, "false")

	inner := newRecordingHandler(nil)
	wrapped := Middleware(WithEnv())(inner)

	if _, ok := wrapped.(*Handler); ok {
		t.Fatalf("Middleware returned async Handler when disabled via env")
	}

	logger := slog.New(wrapped)
	logger.Info("hello")

	if len(inner.state.records) != 1 {
		t.Fatalf("inner handled %d records, want 1", len(inner.state.records))
	}
}

// TestCloseTimesOutWhenWorkersStuck hits the flush timeout branch.
func TestCloseTimesOutWhenWorkersStuck(t *testing.T) {
	t.Parallel()

	block := make(chan struct{})
	inner := newRecordingHandler(block)
	h := Wrap(inner, WithFlushTimeout(20*time.Millisecond))

	if err := h.Handle(context.Background(), slog.NewRecord(time.Now(), slog.LevelInfo, "msg", 0)); err != nil {
		t.Fatalf("Handle returned %v, want nil", err)
	}

	start := time.Now()
	err := h.(*Handler).Close()
	if err == nil {
		t.Fatalf("Handler.Close() returned nil, want timeout error")
	}
	if !errors.Is(err, ErrFlushTimeout) {
		t.Fatalf("Handler.Close() returned %v, want ErrFlushTimeout", err)
	}
	if elapsed := time.Since(start); elapsed < 15*time.Millisecond {
		t.Fatalf("Close returned too quickly: %v", elapsed)
	}
	close(block)
}

// TestWrapDisabledReturnsInner ensures WithEnabled(false) bypasses async wrapping.
func TestWrapDisabledReturnsInner(t *testing.T) {
	inner := newRecordingHandler(nil)
	wrapped := Wrap(inner, WithEnabled(false))
	if wrapped != inner {
		t.Fatalf("Wrap returned %T, want original handler", wrapped)
	}
}

// TestHandleDropsAfterClose exercises Handle when the handler is already closed.
func TestHandleDropsAfterClose(t *testing.T) {
	var dropped []string
	inner := newRecordingHandler(nil)
	h := Wrap(inner, WithOnDrop(func(_ context.Context, r slog.Record) {
		dropped = append(dropped, r.Message)
	}))

	if err := h.(*Handler).Close(); err != nil {
		t.Fatalf("Close() returned %v, want nil", err)
	}

	if err := h.Handle(context.Background(), slog.NewRecord(time.Now(), slog.LevelInfo, "late", 0)); err != nil {
		t.Fatalf("Handle returned %v, want nil", err)
	}

	if len(dropped) != 1 || dropped[0] != "late" {
		t.Fatalf("dropped %v, want [late]", dropped)
	}
	if len(inner.state.records) != 0 {
		t.Fatalf("inner handled %d records after close, want 0", len(inner.state.records))
	}
}

// TestEnqueueClosedChannelRecovery covers the panic recovery path when the queue is closed unexpectedly.
func TestEnqueueClosedChannelRecovery(t *testing.T) {
	var dropped []string
	inner := newRecordingHandler(nil)
	h := Wrap(inner, WithOnDrop(func(_ context.Context, r slog.Record) {
		dropped = append(dropped, r.Message)
	}))

	handler := h.(*Handler)
	close(handler.state.queue)

	if err := h.Handle(context.Background(), slog.NewRecord(time.Now(), slog.LevelInfo, "recover", 0)); err != nil {
		t.Fatalf("Handle returned %v, want nil", err)
	}

	if len(dropped) != 1 || dropped[0] != "recover" {
		t.Fatalf("dropped %v, want [recover]", dropped)
	}
}

type closeErrorHandler struct {
	*recordingHandler
	err error
}

// Close returns the configured error.
func (c *closeErrorHandler) Close() error {
	c.recordingHandler.Close()
	return c.err
}

type noErrorCloser struct {
	*recordingHandler
}

// Close satisfies the Close() signature without returning an error.
func (c *noErrorCloser) Close() {
	c.recordingHandler.Close()
}

// TestClosePropagatesCloserError ensures closerFor picks the error-returning Close().
func TestClosePropagatesCloserError(t *testing.T) {
	inner := &closeErrorHandler{recordingHandler: newRecordingHandler(nil), err: errors.New("boom")}
	h := Wrap(inner)

	if err := h.(*Handler).Close(); !errors.Is(err, inner.err) {
		t.Fatalf("Close() returned %v, want %v", err, inner.err)
	}
	if inner.state.closeCount != 1 {
		t.Fatalf("Close invoked %d times, want 1", inner.state.closeCount)
	}
}

// TestCloseSupportsNonErrorCloser covers the non-error Close() interface branch.
func TestCloseSupportsNonErrorCloser(t *testing.T) {
	inner := &noErrorCloser{recordingHandler: newRecordingHandler(nil)}
	h := Wrap(inner)

	if err := h.(*Handler).Close(); err != nil {
		t.Fatalf("Close() returned %v, want nil", err)
	}
	if inner.state.closeCount != 1 {
		t.Fatalf("Close invoked %d times, want 1", inner.state.closeCount)
	}
}

// TestBuildConfigClampsInvalidValues validates buildConfig sanitises input.
func TestBuildConfigClampsInvalidValues(t *testing.T) {
	cfg := buildConfig([]Option{
		WithQueueSize(-1),
		WithWorkerCount(0),
		WithBatchSize(0),
	})

	if cfg.QueueSize != defaultQueueSize {
		t.Fatalf("QueueSize = %d, want %d", cfg.QueueSize, defaultQueueSize)
	}
	if cfg.WorkerCount != 1 {
		t.Fatalf("WorkerCount = %d, want 1", cfg.WorkerCount)
	}
	if cfg.BatchSize != 1 {
		t.Fatalf("BatchSize = %d, want 1", cfg.BatchSize)
	}
}

// TestApplyEnvParsesValues covers env overlay behaviour.
func TestApplyEnvParsesValues(t *testing.T) {
	t.Setenv(envAsyncEnabled, "false")
	t.Setenv(envAsyncQueueSize, "5")
	t.Setenv(envAsyncWorkers, "2")
	t.Setenv(envAsyncDropMode, "drop_oldest")
	t.Setenv(envAsyncFlushTimeout, "125ms")

	cfg := Config{
		Enabled:     true,
		QueueSize:   1,
		WorkerCount: 1,
		DropMode:    DropModeBlock,
	}
	applyEnv(&cfg)

	if cfg.Enabled {
		t.Fatalf("Enabled = true, want false")
	}
	if cfg.QueueSize != 5 {
		t.Fatalf("QueueSize = %d, want 5", cfg.QueueSize)
	}
	if cfg.WorkerCount != 2 {
		t.Fatalf("WorkerCount = %d, want 2", cfg.WorkerCount)
	}
	if cfg.DropMode != DropModeDropOldest {
		t.Fatalf("DropMode = %v, want DropModeDropOldest", cfg.DropMode)
	}
	if cfg.FlushTimeout != 125*time.Millisecond {
		t.Fatalf("FlushTimeout = %v, want 125ms", cfg.FlushTimeout)
	}
}

// TestApplyEnvBooleanVariants ensures boolean env parsing accepts yes/on/off/1/0 tokens.
func TestApplyEnvBooleanVariants(t *testing.T) {
	cfg := Config{Enabled: false}

	t.Setenv(envAsyncEnabled, "yes")
	applyEnv(&cfg)
	if !cfg.Enabled {
		t.Fatalf("Enabled = false, want true for yes")
	}

	t.Setenv(envAsyncEnabled, "on")
	applyEnv(&cfg)
	if !cfg.Enabled {
		t.Fatalf("Enabled = false, want true for on")
	}

	t.Setenv(envAsyncEnabled, "0")
	applyEnv(&cfg)
	if cfg.Enabled {
		t.Fatalf("Enabled = true, want false for 0")
	}

	t.Setenv(envAsyncEnabled, "invalid-token")
	cfg.Enabled = true
	applyEnv(&cfg)
	if !cfg.Enabled {
		t.Fatalf("Enabled flipped on invalid token, want unchanged")
	}
}

// TestApplyEnvSupportsOtherDropModes exercises remaining drop mode branches.
func TestApplyEnvSupportsOtherDropModes(t *testing.T) {
	t.Setenv(envAsyncDropMode, "drop_newest")
	cfg := Config{DropMode: DropModeBlock}
	applyEnv(&cfg)
	if cfg.DropMode != DropModeDropNewest {
		t.Fatalf("DropMode = %v, want DropModeDropNewest", cfg.DropMode)
	}

	t.Setenv(envAsyncDropMode, "block")
	applyEnv(&cfg)
	if cfg.DropMode != DropModeBlock {
		t.Fatalf("DropMode = %v, want DropModeBlock", cfg.DropMode)
	}
}

// TestDropOldestOnUnbufferedCullsNewest hits the branch where requeue still fails.
func TestDropOldestOnUnbufferedCullsNewest(t *testing.T) {
	var dropped []string
	inner := newRecordingHandler(nil)
	h := Wrap(inner,
		WithQueueSize(0),
		WithDropMode(DropModeDropOldest),
		WithOnDrop(func(_ context.Context, r slog.Record) {
			dropped = append(dropped, r.Message)
		}),
		withWorkerStarter(func(func()) {}), // prevent workers so sends would block
	)

	_ = h.Handle(context.Background(), slog.NewRecord(time.Now(), slog.LevelInfo, "first", 0))
	_ = h.Handle(context.Background(), slog.NewRecord(time.Now(), slog.LevelInfo, "second", 0))

	if len(dropped) != 2 {
		t.Fatalf("dropped %v, want two drops", dropped)
	}
}

// TestCloseIdempotent exercises the already-closed path.
func TestCloseIdempotent(t *testing.T) {
	h := Wrap(newRecordingHandler(nil))

	if err := h.(*Handler).Close(); err != nil {
		t.Fatalf("first Close returned %v, want nil", err)
	}
	if err := h.(*Handler).Close(); err != nil {
		t.Fatalf("second Close returned %v, want nil", err)
	}
}

// bareHandler adapts recordingHandler without exposing Close.
type bareHandler struct {
	recorder *recordingHandler
}

// Handle forwards to the underlying recorder.
func (b *bareHandler) Handle(ctx context.Context, rec slog.Record) error {
	return b.recorder.Handle(ctx, rec)
}

// Enabled always returns true.
func (b *bareHandler) Enabled(context.Context, slog.Level) bool {
	return true
}

// WithAttrs delegates to the recorder.
func (b *bareHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &bareHandler{recorder: b.recorder.WithAttrs(attrs).(*recordingHandler)}
}

// WithGroup delegates to the recorder.
func (b *bareHandler) WithGroup(name string) slog.Handler {
	return &bareHandler{recorder: b.recorder.WithGroup(name).(*recordingHandler)}
}

// TestCloseWithoutCloser covers the nil closer branch in Close().
func TestCloseWithoutCloser(t *testing.T) {
	inner := &bareHandler{recorder: newRecordingHandler(nil)}
	h := Wrap(inner)

	if err := h.(*Handler).Close(); err != nil {
		t.Fatalf("Close() returned %v, want nil", err)
	}
	if inner.recorder.state.closeCount != 0 {
		t.Fatalf("unexpected closeCount = %d", inner.recorder.state.closeCount)
	}
}

// TestCloseRespectsFlushTimeoutWhenWorkersComplete covers the success path with a timeout set.
func TestCloseRespectsFlushTimeoutWhenWorkersComplete(t *testing.T) {
	inner := newRecordingHandler(nil)
	h := Wrap(inner, WithFlushTimeout(50*time.Millisecond))

	if err := h.Handle(context.Background(), slog.NewRecord(time.Now(), slog.LevelInfo, "ok", 0)); err != nil {
		t.Fatalf("Handle returned %v, want nil", err)
	}

	if err := h.(*Handler).Close(); err != nil {
		t.Fatalf("Close() returned %v, want nil", err)
	}
	if len(inner.state.records) != 1 {
		t.Fatalf("handled %d records, want 1", len(inner.state.records))
	}
}

// TestCloseHandlesNilState covers the early return branch.
func TestCloseHandlesNilState(t *testing.T) {
	h := &Handler{}
	if err := h.Close(); err != nil {
		t.Fatalf("Close() returned %v, want nil", err)
	}
}

// TestCloseWhenAlreadyClosedFlagged ensures CompareAndSwap(false, true) path is skipped safely.
func TestCloseWhenAlreadyClosedFlagged(t *testing.T) {
	state := &asyncState{
		queue: make(chan queuedRecord),
	}
	state.closed.Store(true)

	h := &Handler{state: state}
	if err := h.Close(); err != nil {
		t.Fatalf("Close() returned %v, want nil", err)
	}
}

// TestBatchDrainProcessesBurst exercises the batch drain inner loop including the default branch.
func TestBatchDrainProcessesBurst(t *testing.T) {
	t.Parallel()

	inner := newRecordingHandler(nil)
	h := Wrap(inner,
		WithQueueSize(4),
		WithWorkerCount(1),
		WithBatchSize(3),
	)

	first := slog.NewRecord(time.Now(), slog.LevelInfo, "first", 0)
	second := slog.NewRecord(time.Now(), slog.LevelInfo, "second", 0)

	if err := h.Handle(context.Background(), first); err != nil {
		t.Fatalf("Handle(first) returned %v", err)
	}
	if err := h.Handle(context.Background(), second); err != nil {
		t.Fatalf("Handle(second) returned %v", err)
	}

	if err := h.(*Handler).Close(); err != nil {
		t.Fatalf("Close returned %v", err)
	}

	if got := len(inner.state.records); got != 2 {
		t.Fatalf("handled %d records, want 2", got)
	}
}

// TestWithBatchSizeOptionAppliesValue covers the WithBatchSize option path.
func TestWithBatchSizeOptionAppliesValue(t *testing.T) {
	cfg := buildConfig([]Option{WithBatchSize(5)})
	if cfg.BatchSize != 5 {
		t.Fatalf("BatchSize = %d, want 5", cfg.BatchSize)
	}
}

// TestErrorWriterNilSuppressesLogOutput hits the nil errWriter branch in workers.
func TestErrorWriterNilSuppressesLogOutput(t *testing.T) {
	t.Parallel()

	inner := &erroringHandler{
		recordingHandler: newRecordingHandler(nil),
		err:              errors.New("fail"),
	}

	h := Wrap(inner,
		WithQueueSize(1),
		WithErrorWriter(nil),
	)

	if err := h.Handle(context.Background(), slog.NewRecord(time.Now(), slog.LevelInfo, "msg", 0)); err != nil {
		t.Fatalf("Handle returned %v", err)
	}
	if err := h.(*Handler).Close(); err != nil {
		t.Fatalf("Close returned %v", err)
	}
	if got := len(inner.state.records); got != 1 {
		t.Fatalf("handled %d records, want 1", got)
	}
}

// TestBatchLoopDefaultBranch covers the empty-queue default branch in the batch drain loop.
func TestBatchLoopDefaultBranch(t *testing.T) {
	t.Parallel()

	start := make(chan func(), 1)
	inner := newRecordingHandler(nil)

	h := Wrap(inner,
		WithQueueSize(1),
		WithWorkerCount(1),
		WithBatchSize(2),
		withWorkerStarter(func(run func()) {
			start <- run
		}),
	)

	if err := h.Handle(context.Background(), slog.NewRecord(time.Now(), slog.LevelInfo, "only", 0)); err != nil {
		t.Fatalf("Handle returned %v", err)
	}

	run := <-start
	run()

	waitForCalls(t, inner.state.calls, 1)

	if err := h.(*Handler).Close(); err != nil {
		t.Fatalf("Close returned %v", err)
	}
	if got := len(inner.state.records); got != 1 {
		t.Fatalf("handled %d records, want 1", got)
	}
}
