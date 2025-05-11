package slogcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// captureOutput redirects the given *os.File (stdout or stderr), runs action, and returns captured text.
func captureOutput(t *testing.T, target **os.File, action func()) string {
	t.Helper()
	original := *target
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("captureOutput: failed to create pipe: %v", err)
	}
	*target = w

	t.Cleanup(func() {
		_ = w.Close()
		*target = original
	})

	outC := make(chan string)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		outC <- buf.String()
	}()

	action()
	_ = w.Close()
	return <-outC
}

// TestStdoutTarget verifies structured JSON logging to stdout.
func TestStdoutTarget(t *testing.T) {
	output := captureOutput(t, &os.Stdout, func() {
		logger, err := New(WithLogTarget(LogTargetStdout))
		if err != nil {
			t.Fatalf("New stdout error: %v", err)
		}
		logger.InfoContext(context.Background(), "stdout test", "key", "value")
	})

	line := strings.TrimSpace(output)
	if line == "" {
		t.Fatal("captured stdout is empty")
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(line), &payload); err != nil {
		t.Fatalf("failed to unmarshal stdout JSON: %v\n%s", err, line)
	}

	want := map[string]any{"severity": "INFO", "msg": "stdout test", "key": "value"}
	if diff := cmp.Diff(want, payload, cmpopts.IgnoreMapEntries(func(k string, v any) bool { return k == "time" })); diff != "" {
		t.Errorf("stdout payload mismatch (-want +got):\n%s", diff)
	}
}

// TestStderrTarget verifies structured JSON logging to stderr.
func TestStderrTarget(t *testing.T) {
	output := captureOutput(t, &os.Stderr, func() {
		logger, err := New(WithLogTarget(LogTargetStderr))
		if err != nil {
			t.Fatalf("New stderr error: %v", err)
		}
		logger.WarnContext(context.Background(), "stderr test", slog.Int("code", 123))
	})

	// scan for JSON line
	scanner := bufio.NewScanner(strings.NewReader(output))
	var line string
	for scanner.Scan() {
		text := scanner.Text()
		var tmp map[string]any
		if err := json.Unmarshal([]byte(text), &tmp); err == nil {
			line = text
			break
		}
	}
	if line == "" {
		t.Fatalf("no JSON found in stderr output:\n%s", output)
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(line), &payload); err != nil {
		t.Fatalf("unmarshal stderr JSON: %v", err)
	}

	want := map[string]any{"severity": "WARN", "msg": "stderr test", "code": float64(123)}
	if diff := cmp.Diff(want, payload, cmpopts.IgnoreMapEntries(func(k string, v any) bool { return k == "time" })); diff != "" {
		t.Errorf("stderr payload mismatch (-want +got):\n%s", diff)
	}
}

// TestFileTarget writes logs to a file and reads them back.
func TestFileTarget(t *testing.T) {
	d := t.TempDir()
	path := d + "/test.log"
	logger, err := New(WithRedirectToFile(path))
	if err != nil {
		t.Fatalf("New file target error: %v", err)
	}
	defer logger.Close()

	logger.ErrorContext(context.Background(), "file test", "flag", true)
	// read file
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	line := strings.TrimSpace(string(data))
	if line == "" {
		t.Fatal("log file is empty")
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(line), &payload); err != nil {
		t.Fatalf("unmarshal file JSON: %v", err)
	}
	want := map[string]any{"severity": "ERROR", "msg": "file test", "flag": true}
	if diff := cmp.Diff(want, payload, cmpopts.IgnoreMapEntries(func(k string, v any) bool { return k == "time" })); diff != "" {
		t.Errorf("file payload mismatch (-want +got):\n%s", diff)
	}
}

// TestCloseFlush checks that Flush and Close on non-GCP writers are no-ops.
func TestCloseFlush(t *testing.T) {
	logger, err := New(WithLogTarget(LogTargetStdout))
	if err != nil {
		t.Fatalf("New error: %v", err)
	}
	if err := logger.Flush(); err != nil {
		t.Errorf("Flush() = %v, want nil", err)
	}
	if err := logger.Close(); err != nil {
		t.Errorf("Close() = %v, want nil", err)
	}
}

// TestLevelMethods verifies SetLevel and GetLevel behavior.
func TestLevelMethods(t *testing.T) {
	logger, err := New(WithLogTarget(LogTargetStdout), WithLevel(slog.LevelWarn))
	if err != nil {
		t.Fatalf("New error: %v", err)
	}
	if lvl := logger.GetLevel(); lvl != slog.LevelWarn {
		t.Errorf("GetLevel() = %v, want %v", lvl, slog.LevelWarn)
	}
	logger.SetLevel(slog.LevelDebug)
	if lvl := logger.GetLevel(); lvl != slog.LevelDebug {
		t.Errorf("GetLevel() after SetLevel = %v, want %v", lvl, slog.LevelDebug)
	}
}

// TestProjectID verifies ProjectID from option and environment.
func TestProjectID(t *testing.T) {
	// via option
	optID := "opt-proj"
	lg, err := New(WithProjectID(optID), WithLogTarget(LogTargetStdout))
	if err != nil {
		t.Fatalf("New error: %v", err)
	}
	if got := lg.ProjectID(); got != optID {
		t.Errorf("ProjectID() = %q, want %q", got, optID)
	}

	// via env
	envID := "env-proj"
	t.Setenv("GOOGLE_CLOUD_PROJECT", envID)
	lg2, err := New(WithLogTarget(LogTargetStdout))
	if err != nil {
		t.Fatalf("New error: %v", err)
	}
	if got := lg2.ProjectID(); got != envID {
		t.Errorf("ProjectID() from env = %q, want %q", got, envID)
	}
}

// TestConvenienceMethods ensures GCP-level shorthands log correctly in JSON.
func TestConvenienceMethods(t *testing.T) {
	logger, err := New(WithLogTarget(LogTargetStdout), WithLevel(LevelNotice.Level()))
	if err != nil {
		t.Fatalf("New error: %v", err)
	}

	cases := []struct {
		name string
		fn   func()
		want string
	}{
		{"NoticeContext", func() {
			logger.NoticeContext(context.Background(), "note", "a", 1)
		}, "notice"},
		{"CriticalContext", func() {
			logger.CriticalContext(context.Background(), "crit", "b", 2)
		}, "crit"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := captureOutput(t, &os.Stdout, tc.fn)
			var p map[string]any
			if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &p); err != nil {
				t.Fatalf("unmarshal %s: %v", tc.name, err)
			}
			if msg, _ := p["msg"].(string); msg != tc.want {
				t.Errorf("msg = %q, want %q", msg, tc.want)
			}
		})
	}
}

// TestMiddleware applies a simple middleware in stdout mode.
func TestMiddleware(t *testing.T) {
	mw := func(key, val string) Middleware {
		return func(h slog.Handler) slog.Handler {
			return &middlewareTestHandler{next: h, addAttr: func(r *slog.Record) {
				r.AddAttrs(slog.String(key, val))
			}}
		}
	}

	t.Run("StdoutMiddleware", func(t *testing.T) {
		logger, err := New(WithLogTarget(LogTargetStdout), WithMiddleware(mw("x", "v")))
		if err != nil {
			t.Fatalf("New error: %v", err)
		}
		out := captureOutput(t, &os.Stdout, func() {
			logger.Info("hi")
		})
		var p map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &p); err != nil {
			t.Fatalf("unmarshal middleware: %v", err)
		}
		if p["x"] != "v" {
			t.Errorf("middleware attribute = %v, want 'v'", p["x"])
		}
	})
}

// middlewareTestHandler enables injecting attrs before delegation.
type middlewareTestHandler struct {
	next    slog.Handler
	addAttr func(*slog.Record)
}

func (h *middlewareTestHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *middlewareTestHandler) Handle(ctx context.Context, r slog.Record) error {
	if h.addAttr != nil {
		h.addAttr(&r)
	}
	return h.next.Handle(ctx, r)
}

func (h *middlewareTestHandler) WithAttrs(a []slog.Attr) slog.Handler {
	return &middlewareTestHandler{next: h.next.WithAttrs(a), addAttr: h.addAttr}
}

func (h *middlewareTestHandler) WithGroup(name string) slog.Handler {
	return &middlewareTestHandler{next: h.next.WithGroup(name), addAttr: h.addAttr}
}
