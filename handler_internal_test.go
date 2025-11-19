package slogcp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// TestSourceAwareHandlerPropagatesSourceMetadata validates HasSource/With* wrappers.
func TestSourceAwareHandlerPropagatesSourceMetadata(t *testing.T) {
	t.Parallel()

	base := slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{AddSource: true})
	wrapped := sourceAwareHandler{Handler: base}

	if !wrapped.HasSource() {
		t.Fatalf("sourceAwareHandler.HasSource() = false, want true")
	}
	if _, ok := wrapped.WithAttrs(nil).(sourceAwareHandler); !ok {
		t.Fatalf("WithAttrs did not return sourceAwareHandler")
	}
	if _, ok := wrapped.WithGroup("grp").(sourceAwareHandler); !ok {
		t.Fatalf("WithGroup did not return sourceAwareHandler")
	}
}

// TestSourceAwareHandlerHandlesNilChildren confirms the wrapper tolerates nil child handlers.
func TestSourceAwareHandlerHandlesNilChildren(t *testing.T) {
	t.Parallel()

	wrapped := sourceAwareHandler{Handler: nilChildHandler{}}

	if wrapped.WithAttrs(nil) != nil {
		t.Fatalf("WithAttrs on nil child should return nil")
	}
	if wrapped.WithGroup("grp") != nil {
		t.Fatalf("WithGroup on nil child should return nil")
	}
}

// TestHandlerCloseClosesOwnedResources exercises the branches that release switchable writers,
// owned files, and configured closers.
func TestHandlerCloseClosesOwnedResources(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "app.log")
	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("os.OpenFile(%q) = %v", logPath, err)
	}

	switchWriter := &writeCloseSpy{err: errors.New("switch-close")}
	cfgCloser := &closerSpy{}

	h := &Handler{
		Handler:          slog.NewJSONHandler(io.Discard, nil),
		internalLogger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		switchableWriter: NewSwitchableWriter(switchWriter),
		ownedFile:        file,
		cfg:              &handlerConfig{ClosableWriter: cfgCloser},
	}

	if err := h.Close(); !errors.Is(err, switchWriter.err) {
		t.Fatalf("Handler.Close() error = %v, want %v", err, switchWriter.err)
	}
	if switchWriter.closed != 1 {
		t.Fatalf("switch writer close count = %d, want 1", switchWriter.closed)
	}
	if cfgCloser.closed != 1 {
		t.Fatalf("config closer closed = %d, want 1", cfgCloser.closed)
	}
	if _, err := file.WriteString("again"); err == nil {
		t.Fatalf("write to closed file unexpectedly succeeded")
	} else if !errors.Is(err, os.ErrClosed) && !errors.Is(err, os.ErrInvalid) {
		t.Fatalf("write error = %v, want os.ErrClosed or os.ErrInvalid", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("second Handler.Close() returned %v, want nil", err)
	}
}

// TestHandlerCloseReturnsConfigCloserError ensures ClosableWriter errors surface when no other errors occur.
func TestHandlerCloseReturnsConfigCloserError(t *testing.T) {
	t.Parallel()

	cfgCloser := &closerSpy{err: errors.New("config-close")}
	h := &Handler{
		Handler:        slog.NewJSONHandler(io.Discard, nil),
		internalLogger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:            &handlerConfig{ClosableWriter: cfgCloser},
	}

	err := h.Close()
	if !errors.Is(err, cfgCloser.err) {
		t.Fatalf("Handler.Close() error = %v, want %v", err, cfgCloser.err)
	}
	if cfgCloser.closed != 1 {
		t.Fatalf("config closer closed = %d, want 1", cfgCloser.closed)
	}
}

// TestHandlerReopenLogFileSuccess ensures ReopenLogFile rotates descriptors and updates the switchable writer.
func TestHandlerReopenLogFileSuccess(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	logPath := filepath.Join(dir, "app.log")

	oldFile, err := os.CreateTemp(dir, "old-log-*.txt")
	if err != nil {
		t.Fatalf("os.CreateTemp() = %v", err)
	}

	h := &Handler{
		cfg:              &handlerConfig{FilePath: logPath},
		switchableWriter: NewSwitchableWriter(io.Discard),
		internalLogger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		ownedFile:        oldFile,
	}

	if err := h.ReopenLogFile(); err != nil {
		t.Fatalf("ReopenLogFile() returned %v, want nil", err)
	}

	// The previous file should be closed.
	if _, err := oldFile.WriteString("stale"); err == nil {
		t.Fatalf("expected write to closed file to fail")
	}

	currentWriter := h.switchableWriter.GetCurrentWriter()
	file, ok := currentWriter.(*os.File)
	if !ok {
		t.Fatalf("current writer type = %T, want *os.File", currentWriter)
	}
	if file.Name() != logPath {
		t.Fatalf("writer path = %q, want %q", file.Name(), logPath)
	}
	if _, err := file.WriteString("hello\n"); err != nil {
		t.Fatalf("write to reopened file returned %v", err)
	}

	if err := h.Close(); err != nil {
		t.Fatalf("Handler.Close() after reopen returned %v", err)
	}
}

type nilChildHandler struct{}

// Enabled implements slog.Handler.
func (nilChildHandler) Enabled(context.Context, slog.Level) bool { return true }

// Handle implements slog.Handler.
func (nilChildHandler) Handle(context.Context, slog.Record) error { return nil }

// WithAttrs implements slog.Handler and returns nil to simulate handler failure.
func (nilChildHandler) WithAttrs([]slog.Attr) slog.Handler { return nil }

// WithGroup implements slog.Handler and returns nil to simulate handler failure.
func (nilChildHandler) WithGroup(string) slog.Handler { return nil }

type writeCloseSpy struct {
	closed int
	err    error
}

// Write implements io.Writer.
func (w *writeCloseSpy) Write(p []byte) (int, error) { return len(p), nil }

// Close implements io.Closer and records invocations.
func (w *writeCloseSpy) Close() error {
	w.closed++
	return w.err
}

type closerSpy struct {
	closed int
	err    error
}

// Close implements io.Closer and records invocations.
func (c *closerSpy) Close() error {
	c.closed++
	return c.err
}

// TestHandlerReopenLogFileNoopWithoutFile ensures file rotation is a no-op when no file is configured.
func TestHandlerReopenLogFileNoopWithoutFile(t *testing.T) {
	t.Parallel()

	var h Handler
	if err := h.ReopenLogFile(); err != nil {
		t.Fatalf("ReopenLogFile() = %v, want nil", err)
	}
}

// TestHandlerReopenLogFileReportsErrors exercises the error branch used when reopen fails.
func TestHandlerReopenLogFileReportsErrors(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	missingPath := filepath.Join(dir, "missing", "app.log")

	h := &Handler{
		cfg:              &handlerConfig{FilePath: missingPath},
		switchableWriter: NewSwitchableWriter(io.Discard),
		internalLogger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	err := h.ReopenLogFile()
	if err == nil {
		t.Fatalf("ReopenLogFile() = nil, want error")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReopenLogFile() error = %v, want wrapping os.ErrNotExist", err)
	}
}
