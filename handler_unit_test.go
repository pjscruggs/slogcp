//go:build unit
// +build unit

package slogcp_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pjscruggs/slogcp"
)

type closingBuffer struct {
	bytes.Buffer
	closed bool
}

func (c *closingBuffer) Close() error {
	c.closed = true
	return nil
}

func TestNewHandlerWithRedirectWriter(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	h, err := slogcp.NewHandler(io.Discard,
		slogcp.WithLogTarget(slogcp.LogTargetStdout),
		slogcp.WithRedirectWriter(&buf),
	)
	if err != nil {
		t.Fatalf("NewHandler() returned %v, want nil", err)
	}
	t.Cleanup(func() {
		if cerr := h.Close(); cerr != nil {
			t.Errorf("Handler.Close() returned %v, want nil", cerr)
		}
	})

	logger := slog.New(h)
	logger.InfoContext(context.Background(), "hello", slog.String("key", "value"))

	line := buf.String()
	if !strings.Contains(line, `"message":"hello"`) {
		t.Fatalf("log output %q missing message", line)
	}
	if !strings.Contains(line, `"key":"value"`) {
		t.Fatalf("log output %q missing attribute", line)
	}
}

func TestHandlerCloseClosesRedirectWriter(t *testing.T) {
	t.Parallel()

	cw := &closingBuffer{}
	h, err := slogcp.NewHandler(io.Discard,
		slogcp.WithLogTarget(slogcp.LogTargetStdout),
		slogcp.WithRedirectWriter(cw),
	)
	if err != nil {
		t.Fatalf("NewHandler() returned %v, want nil", err)
	}

	if err := h.Close(); err != nil {
		t.Fatalf("Handler.Close() returned %v, want nil", err)
	}
	if !cw.closed {
		t.Fatalf("redirect writer was not closed")
	}
}

func TestHandlerReopenLogFile(t *testing.T) {
	t.Parallel()

	logDir := t.TempDir()
	logPath := filepath.Join(logDir, "app.log")

	h, err := slogcp.NewHandler(io.Discard, slogcp.WithRedirectToFile(logPath))
	if err != nil {
		t.Fatalf("NewHandler() returned %v, want nil", err)
	}
	t.Cleanup(func() {
		if cerr := h.Close(); cerr != nil {
			t.Errorf("Handler.Close() returned %v, want nil", cerr)
		}
	})

	logger := slog.New(h)
	logger.InfoContext(context.Background(), "first")

	if err := h.ReopenLogFile(); err != nil {
		t.Fatalf("ReopenLogFile() returned %v, want nil", err)
	}

	logger.InfoContext(context.Background(), "second")

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile(%q) returned %v", logPath, err)
	}
	content := string(data)
	if !strings.Contains(content, `"message":"first"`) || !strings.Contains(content, `"message":"second"`) {
		t.Fatalf("log file content missing expected entries: %s", content)
	}
}
