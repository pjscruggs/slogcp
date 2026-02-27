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
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
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
	for range n {
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

// TestDropOldestContentionInvariants validates queue/drop accounting under
// producer contention without asserting deterministic eviction identity/order.
func TestDropOldestContentionInvariants(t *testing.T) {
	var dropped atomic.Int64
	block := make(chan struct{})
	inner := newRecordingHandler(block)
	handler := Wrap(inner,
		WithQueueSize(8),
		WithWorkerCount(1),
		WithDropMode(DropModeDropOldest),
		WithOnDrop(func(_ context.Context, rec slog.Record) {
			dropped.Add(1)
			_ = rec // ensure rec.Clone() is exercised on the drop path
		}),
	)

	const (
		producers   = 16
		perProducer = 40
	)
	total := producers * perProducer

	rec := slog.NewRecord(time.Now(), slog.LevelInfo, "contended", 0)
	errCh := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(producers)
	for range producers {
		go func() {
			defer wg.Done()
			for range perProducer {
				if err := handler.Handle(context.Background(), rec); err != nil {
					select {
					case errCh <- err:
					default:
					}
					return
				}
			}
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Handle blocked unexpectedly under DropModeDropOldest contention")
	}

	select {
	case err := <-errCh:
		t.Fatalf("Handle returned %v, want nil", err)
	default:
	}

	close(block)
	if err := handler.(*Handler).Close(); err != nil {
		t.Fatalf("Close returned %v, want nil", err)
	}

	handled := len(inner.state.records)
	droppedCount := int(dropped.Load())

	if handled+droppedCount != total {
		t.Fatalf("handled+dropped = %d, total = %d", handled+droppedCount, total)
	}
	if droppedCount == 0 {
		t.Fatalf("expected dropped records under DropModeDropOldest contention")
	}
}

// TestWorkerBatchDrainProcessesBufferedRecords verifies workers drain additional
// queued records in the same wake-up when BatchSize > 1.
func TestWorkerBatchDrainProcessesBufferedRecords(t *testing.T) {
	t.Parallel()

	start := make(chan func(), 1)
	inner := newRecordingHandler(nil)
	h := Wrap(inner,
		WithQueueSize(4),
		WithBatchSize(3),
		withWorkerStarter(func(run func()) {
			start <- run
		}),
	)

	for _, msg := range []string{"first", "second", "third"} {
		rec := slog.NewRecord(time.Now(), slog.LevelInfo, msg, 0)
		if err := h.Handle(context.Background(), rec); err != nil {
			t.Fatalf("Handle(%q) returned %v, want nil", msg, err)
		}
	}

	run := <-start
	run()
	waitForCalls(t, inner.state.calls, 3)

	if err := h.(*Handler).Close(); err != nil {
		t.Fatalf("Close() returned %v, want nil", err)
	}

	if got := len(inner.state.records); got != 3 {
		t.Fatalf("inner handled %d records, want 3", got)
	}
	for i, want := range []string{"first", "second", "third"} {
		if got := inner.state.records[i].Message; got != want {
			t.Fatalf("record %d message = %q, want %q", i, got, want)
		}
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

	for range total {
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
	if out := buf.String(); !strings.Contains(out, "recovered panic") || !strings.Contains(out, "boom") || !strings.Contains(out, "goroutine ") {
		t.Fatalf("panic output = %q, want recovered panic notice with stack trace", out)
	}
}

// TestWorkerPanicInvokesOnDrop ensures panic-recovered records are reported through OnDrop.
func TestWorkerPanicInvokesOnDrop(t *testing.T) {
	t.Parallel()

	inner := &panicOnceHandler{recordingHandler: newRecordingHandler(nil)}
	var dropped []string
	h := Wrap(inner,
		WithQueueSize(2),
		WithOnDrop(func(_ context.Context, rec slog.Record) {
			dropped = append(dropped, rec.Message)
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
	if err := h.(*Handler).Close(); err != nil {
		t.Fatalf("Close returned %v, want nil", err)
	}

	if len(dropped) != 1 || dropped[0] != "first" {
		t.Fatalf("dropped = %v, want [first]", dropped)
	}
	if got := len(inner.state.records); got != 1 || inner.state.records[0].Message != "second" {
		t.Fatalf("handled records = %v, want only [second]", inner.state.records)
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

// TestCloseUnblocksBlockedHandle ensures Close releases blocked block-mode senders.
func TestCloseUnblocksBlockedHandle(t *testing.T) {
	t.Parallel()

	block := make(chan struct{})
	inner := newRecordingHandler(block)

	var (
		dropMu  sync.Mutex
		dropped []string
	)

	h := Wrap(inner,
		WithQueueSize(1),
		WithWorkerCount(1),
		WithFlushTimeout(20*time.Millisecond),
		WithOnDrop(func(_ context.Context, rec slog.Record) {
			dropMu.Lock()
			dropped = append(dropped, rec.Message)
			dropMu.Unlock()
		}),
	)

	if err := h.Handle(context.Background(), slog.NewRecord(time.Now(), slog.LevelInfo, "first", 0)); err != nil {
		t.Fatalf("Handle(first) returned %v, want nil", err)
	}
	if err := h.Handle(context.Background(), slog.NewRecord(time.Now(), slog.LevelInfo, "second", 0)); err != nil {
		t.Fatalf("Handle(second) returned %v, want nil", err)
	}

	thirdDone := make(chan error, 1)
	go func() {
		thirdDone <- h.Handle(context.Background(), slog.NewRecord(time.Now(), slog.LevelInfo, "third", 0))
	}()

	select {
	case err := <-thirdDone:
		t.Fatalf("third Handle returned early with %v, want blocked send until Close starts", err)
	case <-time.After(20 * time.Millisecond):
	}

	closeDone := make(chan error, 1)
	go func() {
		closeDone <- h.(*Handler).Close()
	}()

	select {
	case err := <-thirdDone:
		if err != nil {
			t.Fatalf("third Handle returned %v, want nil", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("third Handle remained blocked after Close started")
	}

	var closeErr error
	select {
	case closeErr = <-closeDone:
	case <-time.After(300 * time.Millisecond):
		t.Fatalf("Close did not return in time")
	}

	if !errors.Is(closeErr, ErrFlushTimeout) {
		t.Fatalf("Close error = %v, want ErrFlushTimeout", closeErr)
	}

	close(block)

	dropMu.Lock()
	gotDropped := append([]string(nil), dropped...)
	dropMu.Unlock()

	found := slices.Contains(gotDropped, "third")
	if !found {
		t.Fatalf("dropped = %v, want contains third", gotDropped)
	}
}

// TestCloseTimeoutDefersCloserUntilWorkersExit ensures timeout does not race downstream Close.
func TestCloseTimeoutDefersCloserUntilWorkersExit(t *testing.T) {
	t.Parallel()

	block := make(chan struct{})
	inner := &closeErrorHandler{
		recordingHandler: newRecordingHandler(block),
		err:              errors.New("sink-close"),
	}
	h := Wrap(inner, WithFlushTimeout(20*time.Millisecond))

	if err := h.Handle(context.Background(), slog.NewRecord(time.Now(), slog.LevelInfo, "msg", 0)); err != nil {
		t.Fatalf("Handle returned %v, want nil", err)
	}

	err := h.(*Handler).Close()
	if err == nil {
		t.Fatalf("Close() returned nil, want timeout error")
	}
	if !errors.Is(err, ErrFlushTimeout) {
		t.Fatalf("Close() error = %v, want ErrFlushTimeout", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close() error = %v, want context deadline exceeded", err)
	}
	if errors.Is(err, inner.err) {
		t.Fatalf("Close() error = %v, should not include sink close error while workers are still running", err)
	}
	if inner.state.closeCount != 0 {
		t.Fatalf("inner Close() invoked early (%d), want 0 before workers exit", inner.state.closeCount)
	}
	close(block)

	if err := h.(*Handler).Shutdown(context.Background()); !errors.Is(err, inner.err) {
		t.Fatalf("Shutdown() after worker release = %v, want sink close error %v", err, inner.err)
	}
	if inner.state.closeCount != 1 {
		t.Fatalf("inner Close() invoked %d times, want 1", inner.state.closeCount)
	}
}

// TestWrapDisabledReturnsInner ensures WithEnabled(false) bypasses async wrapping.
func TestWrapDisabledReturnsInner(t *testing.T) {
	inner := newRecordingHandler(nil)
	wrapped := Wrap(inner, WithEnabled(false))
	if wrapped != inner {
		t.Fatalf("Wrap returned %T, want original handler", wrapped)
	}
}

// TestHandleProcessesCanceledContext ensures canceled contexts still produce records.
func TestHandleProcessesCanceledContext(t *testing.T) {
	t.Parallel()

	type markerKey struct{}

	inner := newRecordingHandler(nil)
	h := Wrap(inner)

	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), markerKey{}, "present"))
	cancel()

	if err := h.Handle(ctx, slog.NewRecord(time.Now(), slog.LevelInfo, "canceled", 0)); err != nil {
		t.Fatalf("Handle returned %v, want nil", err)
	}
	if err := h.(*Handler).Close(); err != nil {
		t.Fatalf("Close() returned %v, want nil", err)
	}

	if got := len(inner.state.records); got != 1 {
		t.Fatalf("handled records = %d, want 1", got)
	}
	if got := len(inner.state.contexts); got != 1 {
		t.Fatalf("handled contexts = %d, want 1", got)
	}
	if err := inner.state.contexts[0].Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("context error = %v, want context.Canceled", err)
	}
	if got := inner.state.contexts[0].Value(markerKey{}); got != "present" {
		t.Fatalf("context value = %v, want present", got)
	}
}

// TestDetachedContextUsesBackground verifies detaching drops request values and cancellation.
func TestDetachedContextUsesBackground(t *testing.T) {
	t.Parallel()

	type markerKey struct{}

	inner := newRecordingHandler(nil)
	h := Wrap(inner, WithDetachedContext(true))

	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), markerKey{}, "present"))
	cancel()

	if err := h.Handle(ctx, slog.NewRecord(time.Now(), slog.LevelInfo, "detached", 0)); err != nil {
		t.Fatalf("Handle returned %v, want nil", err)
	}
	if err := h.(*Handler).Close(); err != nil {
		t.Fatalf("Close() returned %v, want nil", err)
	}

	if got := len(inner.state.records); got != 1 {
		t.Fatalf("handled records = %d, want 1", got)
	}
	if got := len(inner.state.contexts); got != 1 {
		t.Fatalf("handled contexts = %d, want 1", got)
	}
	if err := inner.state.contexts[0].Err(); err != nil {
		t.Fatalf("detached context error = %v, want nil", err)
	}
	if got := inner.state.contexts[0].Value(markerKey{}); got != nil {
		t.Fatalf("detached context value = %v, want nil", got)
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
	_ = c.recordingHandler.Close()
	return c.err
}

type noErrorCloser struct {
	*recordingHandler
}

// Close satisfies the Close() signature without returning an error.
func (c *noErrorCloser) Close() {
	_ = c.recordingHandler.Close()
}

type errorAborter struct {
	*recordingHandler
	release    chan struct{}
	entered    chan struct{}
	err        error
	abortCount *atomic.Int64
}

// newErrorAborter builds a handler exposing Abort() error for abort-path tests.
func newErrorAborter(err error) *errorAborter {
	release := make(chan struct{})
	return &errorAborter{
		recordingHandler: newRecordingHandler(release),
		release:          release,
		entered:          make(chan struct{}, 32),
		err:              err,
		abortCount:       &atomic.Int64{},
	}
}

// Handle signals entry before delegating to the blocking recorder.
func (h *errorAborter) Handle(ctx context.Context, rec slog.Record) error {
	select {
	case h.entered <- struct{}{}:
	default:
	}
	return h.recordingHandler.Handle(ctx, rec)
}

// Abort releases any blocked Handle call and returns the configured error.
func (h *errorAborter) Abort() error {
	h.abortCount.Add(1)
	select {
	case <-h.release:
	default:
		close(h.release)
	}
	return h.err
}

// WithAttrs preserves the abort behavior for derived handlers.
func (h *errorAborter) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &errorAborter{
		recordingHandler: h.recordingHandler.WithAttrs(attrs).(*recordingHandler),
		release:          h.release,
		entered:          h.entered,
		err:              h.err,
		abortCount:       h.abortCount,
	}
}

// WithGroup preserves the abort behavior for derived handlers.
func (h *errorAborter) WithGroup(name string) slog.Handler {
	return &errorAborter{
		recordingHandler: h.recordingHandler.WithGroup(name).(*recordingHandler),
		release:          h.release,
		entered:          h.entered,
		err:              h.err,
		abortCount:       h.abortCount,
	}
}

type noErrorAborter struct {
	*recordingHandler
	abortCount *atomic.Int64
}

// newNoErrorAborter builds a handler exposing Abort() without an error result.
func newNoErrorAborter() *noErrorAborter {
	return &noErrorAborter{
		recordingHandler: newRecordingHandler(nil),
		abortCount:       &atomic.Int64{},
	}
}

// Abort satisfies the non-error aborter branch used by aborterFor.
func (h *noErrorAborter) Abort() {
	h.abortCount.Add(1)
}

type blockingAborter struct {
	*recordingHandler
	entered    chan struct{}
	release    chan struct{}
	abortCount *atomic.Int64
}

// newBlockingAborter builds a handler whose Abort blocks until release closes.
func newBlockingAborter() *blockingAborter {
	return &blockingAborter{
		recordingHandler: newRecordingHandler(nil),
		entered:          make(chan struct{}, 1),
		release:          make(chan struct{}),
		abortCount:       &atomic.Int64{},
	}
}

// Abort records invocation, signals entry, and blocks until explicitly released.
func (h *blockingAborter) Abort() error {
	h.abortCount.Add(1)
	select {
	case h.entered <- struct{}{}:
	default:
	}
	<-h.release
	return nil
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

// TestShutdownTimeoutReturnsDeadline verifies Shutdown surfaces timeout context errors.
func TestShutdownTimeoutReturnsDeadline(t *testing.T) {
	t.Parallel()

	block := make(chan struct{})
	inner := newRecordingHandler(block)
	h := Wrap(inner)

	if err := h.Handle(context.Background(), slog.NewRecord(time.Now(), slog.LevelInfo, "msg", 0)); err != nil {
		t.Fatalf("Handle returned %v, want nil", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := h.(*Handler).Shutdown(ctx)
	if err == nil {
		t.Fatalf("Shutdown returned nil, want timeout")
	}
	if !errors.Is(err, ErrFlushTimeout) {
		t.Fatalf("Shutdown error = %v, want ErrFlushTimeout", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown error = %v, want context deadline exceeded", err)
	}
	if inner.state.closeCount != 0 {
		t.Fatalf("inner Close() invoked %d times before worker completion, want 0", inner.state.closeCount)
	}

	close(block)
	if err := h.(*Handler).Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown after release returned %v, want nil", err)
	}
	if inner.state.closeCount != 1 {
		t.Fatalf("inner Close() invoked %d times after completion, want 1", inner.state.closeCount)
	}
}

// TestAbortDropsQueuedRecords verifies Abort drops queued records and invokes the abort hook.
func TestAbortDropsQueuedRecords(t *testing.T) {
	t.Parallel()

	inner := newErrorAborter(errors.New("abort-hook"))
	var (
		dropMu  sync.Mutex
		dropped []string
	)

	h := Wrap(inner,
		WithQueueSize(8),
		WithWorkerCount(1),
		WithOnDrop(func(_ context.Context, rec slog.Record) {
			dropMu.Lock()
			dropped = append(dropped, rec.Message)
			dropMu.Unlock()
		}),
	)

	for _, msg := range []string{"first", "second", "third"} {
		rec := slog.NewRecord(time.Now(), slog.LevelInfo, msg, 0)
		if err := h.Handle(context.Background(), rec); err != nil {
			t.Fatalf("Handle(%q) returned %v, want nil", msg, err)
		}
	}

	select {
	case <-inner.entered:
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("timed out waiting for worker to enter Handle")
	}

	err := h.(*Handler).Abort()
	if !errors.Is(err, ErrAborted) {
		t.Fatalf("Abort() error = %v, want ErrAborted", err)
	}
	if !errors.Is(err, inner.err) {
		t.Fatalf("Abort() error = %v, want abort hook error %v", err, inner.err)
	}
	if got := inner.abortCount.Load(); got != 1 {
		t.Fatalf("abort hook called %d times, want 1", got)
	}
	if got := len(inner.state.records); got != 1 {
		t.Fatalf("handled records = %d, want 1 in-flight record", got)
	}
	if got := inner.state.records[0].Message; got != "first" {
		t.Fatalf("handled message = %q, want first", got)
	}
	if inner.state.closeCount != 1 {
		t.Fatalf("inner Close() invoked %d times, want 1", inner.state.closeCount)
	}

	dropMu.Lock()
	gotDropped := append([]string(nil), dropped...)
	dropMu.Unlock()
	if !slices.Contains(gotDropped, "second") || !slices.Contains(gotDropped, "third") {
		t.Fatalf("dropped = %v, want contains second and third", gotDropped)
	}

	if err := h.(*Handler).Shutdown(context.Background()); !errors.Is(err, ErrAborted) {
		t.Fatalf("Shutdown after Abort = %v, want ErrAborted", err)
	}
}

// TestAbortSupportsNonErrorAborter covers the Abort() interface branch without error return.
func TestAbortSupportsNonErrorAborter(t *testing.T) {
	t.Parallel()

	inner := newNoErrorAborter()
	h := Wrap(inner)

	if err := h.(*Handler).Abort(); !errors.Is(err, ErrAborted) {
		t.Fatalf("Abort() error = %v, want ErrAborted", err)
	}
	if got := inner.abortCount.Load(); got != 1 {
		t.Fatalf("abort hook called %d times, want 1", got)
	}
}

// TestAbortWithoutAborterStillMarksAborted ensures nil abort hooks still return ErrAborted.
func TestAbortWithoutAborterStillMarksAborted(t *testing.T) {
	t.Parallel()

	h := Wrap(newRecordingHandler(nil))
	if err := h.(*Handler).Abort(); !errors.Is(err, ErrAborted) {
		t.Fatalf("Abort() error = %v, want ErrAborted", err)
	}
}

// TestAbortContextTimeoutStartsAbortOnce ensures repeated bounded abort waits
// do not spawn repeated downstream abort attempts.
func TestAbortContextTimeoutStartsAbortOnce(t *testing.T) {
	t.Parallel()

	inner := newBlockingAborter()
	h := Wrap(inner)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	for range 5 {
		err := h.(*Handler).AbortContext(ctx)
		if err == nil {
			t.Fatalf("AbortContext() returned nil, want timeout")
		}
		if !errors.Is(err, ErrAborted) {
			t.Fatalf("AbortContext() error = %v, want ErrAborted", err)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("AbortContext() error = %v, want context deadline exceeded", err)
		}
	}

	select {
	case <-inner.entered:
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("timed out waiting for downstream abort to start")
	}
	if got := inner.abortCount.Load(); got != 1 {
		t.Fatalf("downstream abort invoked %d times, want 1", got)
	}

	close(inner.release)
	if err := h.(*Handler).AbortContext(context.Background()); !errors.Is(err, ErrAborted) {
		t.Fatalf("AbortContext(background) error = %v, want ErrAborted", err)
	}
}

// TestAbortContextWithoutDeadlineUsesCallerAbort verifies contexts without a
// deadline are treated as an unbounded wait for the caller.
func TestAbortContextWithoutDeadlineUsesCallerAbort(t *testing.T) {
	t.Parallel()

	inner := newNoErrorAborter()
	h := Wrap(inner)

	if err := h.(*Handler).AbortContext(context.TODO()); !errors.Is(err, ErrAborted) {
		t.Fatalf("AbortContext(context.TODO()) error = %v, want ErrAborted", err)
	}
	if got := inner.abortCount.Load(); got != 1 {
		t.Fatalf("abort hook called %d times, want 1", got)
	}
}

// TestAbortCoordinationDefensiveBranches exercises guard rails that are only
// reachable in malformed/internal states.
func TestAbortCoordinationDefensiveBranches(t *testing.T) {
	t.Parallel()

	// Nil state: startAbort returns an already-closed done channel and
	// abortOutcome reports nil to match other nil-receiver helpers.
	var nilState *asyncState
	select {
	case <-nilState.startAbort():
	default:
		t.Fatalf("nil startAbort should return closed channel")
	}
	if err := nilState.abortOutcome(); err != nil {
		t.Fatalf("nil abortOutcome = %v, want nil", err)
	}

	// If abortStartOnce is already consumed (unexpected/malformed), startAbort
	// must still return a closed channel instead of blocking forever.
	malformed := &asyncState{}
	malformed.abortStartOnce.Do(func() {})
	select {
	case <-malformed.startAbort():
	default:
		t.Fatalf("malformed startAbort should return closed channel")
	}
	if err := malformed.abortOutcome(); !errors.Is(err, ErrAborted) {
		t.Fatalf("malformed abortOutcome = %v, want ErrAborted", err)
	}
}

// TestShutdownAndAbortHandleNilState covers nil guards for exported shutdown methods.
func TestShutdownAndAbortHandleNilState(t *testing.T) {
	t.Parallel()

	h := &Handler{}
	if err := h.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown(context.Background()) returned %v, want nil", err)
	}
	if err := h.Abort(); err != nil {
		t.Fatalf("Abort() returned %v, want nil", err)
	}
}

// TestStartWorkersDoneMonitorIdempotent ensures repeated monitor setup is safe.
func TestStartWorkersDoneMonitorIdempotent(t *testing.T) {
	t.Parallel()

	state := &asyncState{}
	state.startWorkersDoneMonitor()
	state.startWorkersDoneMonitor()

	select {
	case <-state.workersDoneChannel():
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("workersDone channel did not close for zero workers")
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
	if cfg.WorkerCount != defaultWorkerCountBlock {
		t.Fatalf("WorkerCount = %d, want %d", cfg.WorkerCount, defaultWorkerCountBlock)
	}
	if cfg.BatchSize != defaultBatchSizeBlock {
		t.Fatalf("BatchSize = %d, want %d", cfg.BatchSize, defaultBatchSizeBlock)
	}
}

// TestBuildConfigUsesModeDefaults ensures each drop mode picks its tuned baseline.
func TestBuildConfigUsesModeDefaults(t *testing.T) {
	tests := []struct {
		name        string
		opts        []Option
		wantQueue   int
		wantWorkers int
		wantBatch   int
	}{
		{
			name:        "block",
			opts:        nil,
			wantQueue:   defaultQueueSizeBlock,
			wantWorkers: defaultWorkerCountBlock,
			wantBatch:   defaultBatchSizeBlock,
		},
		{
			name:        "drop_newest",
			opts:        []Option{WithDropMode(DropModeDropNewest)},
			wantQueue:   defaultQueueSizeDropNewest,
			wantWorkers: defaultWorkerCountDropNewest,
			wantBatch:   defaultBatchSizeDropNewest,
		},
		{
			name:        "drop_oldest",
			opts:        []Option{WithDropMode(DropModeDropOldest)},
			wantQueue:   defaultQueueSizeDropOldest,
			wantWorkers: defaultWorkerCountDropOldest,
			wantBatch:   defaultBatchSizeDropOldest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := buildConfig(tt.opts)
			if cfg.QueueSize != tt.wantQueue {
				t.Fatalf("QueueSize = %d, want %d", cfg.QueueSize, tt.wantQueue)
			}
			if cfg.WorkerCount != tt.wantWorkers {
				t.Fatalf("WorkerCount = %d, want %d", cfg.WorkerCount, tt.wantWorkers)
			}
			if cfg.BatchSize != tt.wantBatch {
				t.Fatalf("BatchSize = %d, want %d", cfg.BatchSize, tt.wantBatch)
			}
		})
	}
}

// TestBuildConfigUnknownModeFallsBack ensures unknown modes reuse block defaults.
func TestBuildConfigUnknownModeFallsBack(t *testing.T) {
	cfg := buildConfig([]Option{WithDropMode(DropMode(99))})

	if cfg.DropMode != DropMode(99) {
		t.Fatalf("DropMode = %v, want 99", cfg.DropMode)
	}
	if cfg.QueueSize != defaultQueueSizeBlock {
		t.Fatalf("QueueSize = %d, want %d", cfg.QueueSize, defaultQueueSizeBlock)
	}
	if cfg.WorkerCount != defaultWorkerCountBlock {
		t.Fatalf("WorkerCount = %d, want %d", cfg.WorkerCount, defaultWorkerCountBlock)
	}
	if cfg.BatchSize != defaultBatchSizeBlock {
		t.Fatalf("BatchSize = %d, want %d", cfg.BatchSize, defaultBatchSizeBlock)
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

// TestApplyEnvBooleanVariants ensures boolean env parsing accepts strconv.ParseBool tokens.
func TestApplyEnvBooleanVariants(t *testing.T) {
	cfg := Config{Enabled: false}

	t.Setenv(envAsyncEnabled, "true")
	applyEnv(&cfg)
	if !cfg.Enabled {
		t.Fatalf("Enabled = false, want true for true")
	}

	t.Setenv(envAsyncEnabled, "1")
	applyEnv(&cfg)
	if !cfg.Enabled {
		t.Fatalf("Enabled = false, want true for 1")
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

// TestDrainBatchReturnsWhenQueueEmpty covers the empty-queue default branch directly.
func TestDrainBatchReturnsWhenQueueEmpty(t *testing.T) {
	t.Parallel()

	state := &asyncState{
		queue: make(chan queuedRecord, 1),
	}
	state.drainBatch(2)
}

// TestDropNoCallbackIsNoop covers the drop helper when no onDrop callback is configured.
func TestDropNoCallbackIsNoop(t *testing.T) {
	t.Parallel()

	h := &Handler{}
	h.drop(queuedRecord{
		ctx: context.Background(),
		rec: slog.NewRecord(time.Now(), slog.LevelInfo, "noop", 0),
	})
}

// TestEnqueueDropsWhenShutdownSignalClosed verifies all enqueue modes drop immediately once shutdown begins.
func TestEnqueueDropsWhenShutdownSignalClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		mode DropMode
	}{
		{name: "block", mode: DropModeBlock},
		{name: "drop_newest", mode: DropModeDropNewest},
		{name: "drop_oldest", mode: DropModeDropOldest},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var dropped []string
			h := &Handler{
				dropMode: tc.mode,
				onDrop: func(_ context.Context, rec slog.Record) {
					dropped = append(dropped, rec.Message)
				},
				state: &asyncState{
					queue:     make(chan queuedRecord, 1),
					closingCh: make(chan struct{}),
				},
			}
			h.state.gateCond = sync.NewCond(&h.state.gateMu)
			close(h.state.closingCh)

			item := queuedRecord{
				ctx: context.Background(),
				rec: slog.NewRecord(time.Now(), slog.LevelInfo, "shutdown", 0),
			}
			if err := h.enqueue(item); err != nil {
				t.Fatalf("enqueue returned %v, want nil", err)
			}
			if len(dropped) != 1 || dropped[0] != "shutdown" {
				t.Fatalf("dropped = %v, want [shutdown]", dropped)
			}
		})
	}
}

// TestAsyncStateBeginCloseIdempotent verifies beginClose tolerates an already-closed shutdown signal.
func TestAsyncStateBeginCloseIdempotent(t *testing.T) {
	t.Parallel()

	state := &asyncState{
		queue:     make(chan queuedRecord, 1),
		closingCh: make(chan struct{}),
	}
	state.gateCond = sync.NewCond(&state.gateMu)

	state.beginClose()
	state.beginClose()
}

// TestAsyncStateNilHelpersAreSafe exercises nil guard paths on helper methods.
func TestAsyncStateNilHelpersAreSafe(t *testing.T) {
	t.Parallel()

	var nilState *asyncState
	if nilState.beginSend() {
		t.Fatalf("nil beginSend reported true, want false")
	}
	nilState.endSend()
	nilState.endClose()
	nilState.startWorkersDoneMonitor()
	nilState.startShutdown()
	if err := nilState.waitWorkers(context.Background()); err != nil {
		t.Fatalf("nil waitWorkers returned %v, want nil", err)
	}
	if err := nilState.finalizeClose(); err != nil {
		t.Fatalf("nil finalizeClose returned %v, want nil", err)
	}
	if err := nilState.abortDownstream(); err != nil {
		t.Fatalf("nil abortDownstream returned %v, want nil", err)
	}

	empty := &asyncState{}
	empty.endClose()
	empty.startShutdown()
	if err := empty.waitWorkers(context.Background()); err != nil {
		t.Fatalf("empty waitWorkers returned %v, want nil", err)
	}
	if err := empty.finalizeClose(); err != nil {
		t.Fatalf("empty finalizeClose returned %v, want nil", err)
	}
	if err := empty.abortDownstream(); err != nil {
		t.Fatalf("empty abortDownstream returned %v, want nil", err)
	}
}
