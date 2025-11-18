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
