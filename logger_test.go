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

package slogcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"

	"cloud.google.com/go/logging"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/pjscruggs/slogcp/internal/gcp"
)

// mockEntryLogger captures logging.Entry structs.
type mockEntryLogger struct {
	mu      sync.Mutex
	entries []logging.Entry
}

func (m *mockEntryLogger) Log(e logging.Entry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Shallow copy payload map
	if payloadMap, ok := e.Payload.(map[string]any); ok {
		copiedPayload := make(map[string]any, len(payloadMap))
		for k, v := range payloadMap {
			copiedPayload[k] = v
		}
		e.Payload = copiedPayload
	}
	m.entries = append(m.entries, e)
}
func (m *mockEntryLogger) GetEntries() []logging.Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	entriesCopy := make([]logging.Entry, len(m.entries))
	copy(entriesCopy, m.entries)
	return entriesCopy
}
func (m *mockEntryLogger) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = nil
}

// mockClientManager implements the clientManagerInterface for testing.
// It also implements gcp.GcpLoggerAPI to be returned by GetLogger.
type mockClientManager struct {
	mu           sync.Mutex
	initErr      error // Error to return from Initialize()
	closeErr     error // Error to return from Close()
	flushErr     error // Error to return from Flush()
	initCalled   bool
	closeCalled  bool
	flushCalled  bool
	logCalled    bool
	leveler      slog.Leveler
	entryLogger  *mockEntryLogger // Holds the mock logger to capture entries
	lastLogEntry logging.Entry
}

func newMockClientManager(leveler slog.Leveler) *mockClientManager {
	if leveler == nil {
		lv := new(slog.LevelVar)
		lv.Set(slog.LevelInfo) // Default level for mock
		leveler = lv
	}
	return &mockClientManager{
		leveler:     leveler,
		entryLogger: &mockEntryLogger{},
	}
}

func (m *mockClientManager) Initialize() error {
	m.mu.Lock()
	m.initCalled = true
	m.mu.Unlock()
	return m.initErr
}
func (m *mockClientManager) Close() error {
	m.mu.Lock()
	m.closeCalled = true
	m.mu.Unlock()
	// Simulate Flush being called internally by Close if successful
	if m.closeErr == nil {
		_ = m.Flush() // Call internal Flush mock
	}
	return m.closeErr
}

// Flush implements the gcp.GcpLoggerAPI part of the interface.
func (m *mockClientManager) Flush() error {
	m.mu.Lock()
	m.flushCalled = true
	m.mu.Unlock()
	return m.flushErr
}

// Log implements the gcp.GcpLoggerAPI part of the interface.
// Note: The real gcpLoggerAPI Log method doesn't return an error,
// but the mockClientManager's internal Log method might need to
// for other testing purposes. Here we match the interface.
func (m *mockClientManager) Log(e logging.Entry) {
	m.mu.Lock()
	m.logCalled = true
	m.lastLogEntry = e
	m.mu.Unlock()
	m.entryLogger.Log(e)
	// Interface doesn't return error, so we don't either.
}

func (m *mockClientManager) GetLeveler() slog.Leveler {
	return m.leveler
}

// GetLogger implements the clientManagerInterface.
// It returns the mock manager itself if initialized, as the mock
// manager also fulfills the gcp.GcpLoggerAPI interface.
func (m *mockClientManager) GetLogger() (gcp.GcpLoggerAPI, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.initErr != nil {
		return nil, m.initErr // Return init error if initialization failed
	}
	// In this mock setup, the manager itself acts as the logger API
	return m, nil
}

func (m *mockClientManager) WasInitCalled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.initCalled
}
func (m *mockClientManager) WasCloseCalled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closeCalled
}
func (m *mockClientManager) WasFlushCalled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.flushCalled
}
func (m *mockClientManager) GetCapturedEntries() []logging.Entry {
	return m.entryLogger.GetEntries()
}
func (m *mockClientManager) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.initCalled = false
	m.closeCalled = false
	m.flushCalled = false
	m.logCalled = false
	m.initErr = nil
	m.closeErr = nil
	m.flushErr = nil
	m.entryLogger.Reset()
}

// Compile-time check that the mock satisfies the exported interface.
var _ clientManagerInterface = (*mockClientManager)(nil)

// Compile-time check that the mock also satisfies the logger API interface it returns.
var _ gcp.GcpLoggerAPI = (*mockClientManager)(nil)

// captureOutput redirects os.Stdout or os.Stderr, runs the provided function,
// captures the output, and restores the original stream.
// Tests using this cannot run in parallel.
func captureOutput(t *testing.T, stream **os.File, action func()) string {
	t.Helper()
	original := *stream
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("captureOutput: Failed to create pipe: %v", err)
	}
	*stream = w

	t.Cleanup(func() {
		*stream = original
		if err := r.Close(); err != nil {
			t.Logf("Error closing pipe reader: %v", err)
		}
	})

	outC := make(chan string)
	go func() {
		var buf bytes.Buffer
		_, copyErr := io.Copy(&buf, r)
		// Ignore pipe closed errors, which can happen normally during cleanup.
		if copyErr != nil && !errors.Is(copyErr, os.ErrClosed) && !errors.Is(copyErr, io.ErrClosedPipe) {
			t.Logf("captureOutput: Error copying stream: %v", copyErr)
		}
		outC <- buf.String()
	}()

	action()
	// Close the writer end of the pipe to signal EOF to the reading goroutine.
	// Ignore error as the reader might have already closed the pipe.
	_ = w.Close()

	return <-outC
}

// middlewareTestHandler is a handler wrapper for testing middleware functionality.
// It wraps an existing handler and allows injecting additional behavior.
type middlewareTestHandler struct {
	next    slog.Handler
	addAttr func(*slog.Record)
}

func (h *middlewareTestHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *middlewareTestHandler) Handle(ctx context.Context, r slog.Record) error {
	// Apply the middleware transformation
	if h.addAttr != nil {
		h.addAttr(&r)
	}
	// Delegate to the next handler
	return h.next.Handle(ctx, r)
}

func (h *middlewareTestHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &middlewareTestHandler{
		next:    h.next.WithAttrs(attrs),
		addAttr: h.addAttr,
	}
}

func (h *middlewareTestHandler) WithGroup(name string) slog.Handler {
	return &middlewareTestHandler{
		next:    h.next.WithGroup(name),
		addAttr: h.addAttr,
	}
}

// TestNewLogger verifies the New function's behavior under different configurations.
func TestNewLogger(t *testing.T) {
	// Store original factory and restore it after tests modifying it.
	originalFactory := newClientManagerFunc
	t.Cleanup(func() { newClientManagerFunc = originalFactory })

	t.Run("StdoutTarget", func(t *testing.T) {
		var capturedJSON map[string]any
		mockMgr := newMockClientManager(nil)
		// Override the factory function for this test run.
		newClientManagerFunc = func(cfg gcp.Config, ua string, co gcp.ClientOptions, lv *slog.LevelVar) clientManagerInterface {
			return mockMgr // Return mock if factory is called (it shouldn't be)
		}

		output := captureOutput(t, &os.Stdout, func() {
			logger, err := New(WithLogTarget(LogTargetStdout))
			if err != nil {
				t.Fatalf("New(WithLogTarget(LogTargetStdout)) error = %v, want nil", err)
			}
			if logger.clientMgr != nil {
				t.Error("logger.clientMgr != nil in stdout mode")
			}
			if mockMgr.WasInitCalled() {
				t.Error("mockClientManager.Initialize called unexpectedly in stdout mode")
			}
			logger.InfoContext(context.Background(), "stdout test", "key", "value")
		})

		// Verify JSON output
		trimmedOutput := strings.TrimSpace(output)
		if trimmedOutput == "" {
			t.Fatal("Captured stdout is empty")
		}
		if err := json.Unmarshal([]byte(trimmedOutput), &capturedJSON); err != nil {
			t.Fatalf("Failed to unmarshal stdout JSON: %v\nOutput:\n%s", err, output)
		}
		wantPayload := map[string]any{"severity": "INFO", "msg": "stdout test", "key": "value"}
		if diff := cmp.Diff(wantPayload, capturedJSON, cmpopts.IgnoreMapEntries(func(k string, v any) bool { return k == "time" })); diff != "" {
			t.Errorf("Stdout log mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("StderrTarget", func(t *testing.T) {
		var capturedJSON map[string]any
		mockMgr := newMockClientManager(nil)
		newClientManagerFunc = func(cfg gcp.Config, ua string, co gcp.ClientOptions, lv *slog.LevelVar) clientManagerInterface {
			return mockMgr
		}

		output := captureOutput(t, &os.Stderr, func() {
			logger, err := New(WithLogTarget(LogTargetStderr))
			if err != nil {
				t.Fatalf("New(WithLogTarget(LogTargetStderr)) error = %v, want nil", err)
			}
			if logger.clientMgr != nil {
				t.Error("logger.clientMgr != nil in stderr mode")
			}
			if mockMgr.WasInitCalled() {
				t.Error("mockClientManager.Initialize called unexpectedly in stderr mode")
			}
			logger.WarnContext(context.Background(), "stderr test", slog.Int("code", 123))
		})

		// Parse the output line by line to find valid JSON
		var jsonLine string
		scanner := bufio.NewScanner(strings.NewReader(output))
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "[slogcp] INFO:") {
				// Skip informational lines
				continue
			}

			// Try to unmarshal as JSON
			var tmpJSON map[string]any
			if err := json.Unmarshal([]byte(line), &tmpJSON); err == nil {
				jsonLine = line
				capturedJSON = tmpJSON
				break
			}
		}

		if err := scanner.Err(); err != nil {
			t.Fatalf("Error scanning stderr output: %v", err)
		}

		// Verify JSON was found
		if jsonLine == "" {
			t.Fatalf("No valid JSON found in stderr output:\n%s", output)
		}

		wantPayload := map[string]any{"severity": "WARN", "msg": "stderr test", "code": float64(123)}
		if diff := cmp.Diff(wantPayload, capturedJSON, cmpopts.IgnoreMapEntries(func(k string, v any) bool { return k == "time" })); diff != "" {
			t.Errorf("Stderr log mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("GCPTarget_InitFail_Fallback", func(t *testing.T) {
		initErr := errors.New("mock init failure")
		mockMgr := newMockClientManager(nil)
		mockMgr.initErr = initErr // Configure mock to fail init
		newClientManagerFunc = func(cfg gcp.Config, ua string, co gcp.ClientOptions, lv *slog.LevelVar) clientManagerInterface {
			return mockMgr
		}

		var capturedJSON map[string]any
		var logger *Logger

		stderrOutput := captureOutput(t, &os.Stderr, func() {
			stdoutOutput := captureOutput(t, &os.Stdout, func() {
				var err error
				logger, err = New(WithProjectID("fail-proj")) // Need ProjectID to trigger GCP path
				if err != nil {
					t.Fatalf("New() error = %v on fallback, want nil", err)
				}
				if logger.clientMgr != nil {
					t.Error("logger.clientMgr != nil in fallback mode")
				}
				if !mockMgr.WasInitCalled() {
					t.Error("mockClientManager.Initialize was not called")
				}
				logger.ErrorContext(context.Background(), "fallback log", "reason", "init_fail")
			})
			// Verify fallback JSON output
			trimmedOutput := strings.TrimSpace(stdoutOutput)
			if trimmedOutput == "" {
				t.Fatal("Captured stdout (fallback) is empty")
			}
			if err := json.Unmarshal([]byte(trimmedOutput), &capturedJSON); err != nil {
				t.Fatalf("Failed to unmarshal stdout JSON (fallback): %v\nOutput:\n%s", err, stdoutOutput)
			}
		})

		// Verify stderr warning
		if !strings.Contains(stderrOutput, "Failed to initialize Cloud Logging client") || !strings.Contains(stderrOutput, initErr.Error()) {
			t.Errorf("Expected stderr warning about init failure, got:\n%s", stderrOutput)
		}

		// Verify fallback log content
		wantPayload := map[string]any{"severity": "ERROR", "msg": "fallback log", "reason": "init_fail"}
		if diff := cmp.Diff(wantPayload, capturedJSON, cmpopts.IgnoreMapEntries(func(k string, v any) bool { return k == "time" })); diff != "" {
			t.Errorf("Fallback log mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("GCPTarget_Success", func(t *testing.T) {
		mockMgr := newMockClientManager(nil)
		mockMgr.initErr = nil // Ensure success
		newClientManagerFunc = func(cfg gcp.Config, ua string, co gcp.ClientOptions, lv *slog.LevelVar) clientManagerInterface {
			return mockMgr
		}

		logger, err := New(
			WithProjectID("gcp-success-proj"),
			WithLevel(LevelDebug.Level()),
		)
		if err != nil {
			t.Fatalf("New() error = %v for GCP success, want nil", err)
		}
		if logger.clientMgr == nil {
			t.Fatal("logger.clientMgr == nil in GCP success mode")
		}
		if !mockMgr.WasInitCalled() {
			t.Error("mockClientManager.Initialize was not called")
		}

		// Log a message and verify it was passed to the mock manager's Log method
		ctx := context.Background()
		logger.InfoContext(ctx, "gcp success", "id", 123)

		entries := mockMgr.GetCapturedEntries()
		if len(entries) != 1 {
			t.Fatalf("Expected 1 log entry captured by mock manager, got %d", len(entries))
		}
		entry := entries[0]

		// Verify the captured logging.Entry
		if entry.Severity != logging.Info {
			t.Errorf("Entry severity = %v, want %v", entry.Severity, logging.Info)
		}
		payload, ok := entry.Payload.(map[string]any)
		if !ok {
			t.Fatalf("Entry payload is not map[string]any")
		}
		wantPayload := map[string]any{"message": "gcp success", "id": int64(123)} // JSON number
		optIgnore := cmpopts.IgnoreMapEntries(func(k string, v any) bool { return k == "time" || k == "insertId" })
		if diff := cmp.Diff(wantPayload, payload, optIgnore); diff != "" {
			t.Errorf("GCP log payload mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("GCPTarget_WithAttrsAndGroup", func(t *testing.T) {
		mockMgr := newMockClientManager(nil)
		newClientManagerFunc = func(cfg gcp.Config, ua string, co gcp.ClientOptions, lv *slog.LevelVar) clientManagerInterface {
			return mockMgr
		}

		logger, err := New(
			WithProjectID("gcp-with-proj"),
			WithLevel(LevelDebug.Level()),
			WithAttrs([]slog.Attr{slog.String("common", "val")}), // Attr added at New()
			WithGroup("req"),                                     // Group added at New()
		)
		if err != nil {
			t.Fatalf("New() failed: %v", err)
		}

		// Log message with additional context
		logger.With("id", 123).Info("request processed", "status", 200)

		entries := mockMgr.GetCapturedEntries()
		if len(entries) != 1 {
			t.Fatalf("Expected 1 log entry, got %d", len(entries))
		}
		entry := entries[0]

		if entry.Severity != logging.Info {
			t.Errorf("Entry severity = %v, want %v", entry.Severity, logging.Info)
		}
		payload, ok := entry.Payload.(map[string]any)
		if !ok {
			t.Fatalf("Entry payload is not map[string]any")
		}
		wantPayload := map[string]any{
			"message": "request processed",
			"common":  "val", // From New(WithAttrs)
			"req": map[string]any{ // From New(WithGroup)
				"id":     int64(123), // From logger.With
				"status": int64(200), // From logger.Info
			},
		}
		optIgnore := cmpopts.IgnoreMapEntries(func(k string, v any) bool { return k == "time" || k == "insertId" })
		if diff := cmp.Diff(wantPayload, payload, optIgnore); diff != "" {
			t.Errorf("GCP log payload with initial attrs/group mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("StdoutTarget_WithAttrsAndGroup", func(t *testing.T) {
		var capturedJSON map[string]any
		output := captureOutput(t, &os.Stdout, func() {
			logger, err := New(
				WithLogTarget(LogTargetStdout),
				WithAttrs([]slog.Attr{slog.String("app", "tester")}),
				WithGroup("data"),
			)
			if err != nil {
				t.Fatalf("New() failed: %v", err)
			}
			logger.With("run", 1).Error("test run failed", "code", 500)
		})

		// Verify JSON output
		trimmedOutput := strings.TrimSpace(output)
		if trimmedOutput == "" {
			t.Fatal("Captured stdout is empty")
		}
		if err := json.Unmarshal([]byte(trimmedOutput), &capturedJSON); err != nil {
			t.Fatalf("Failed to unmarshal stdout JSON: %v\nOutput:\n%s", err, output)
		}
		wantPayload := map[string]any{
			"severity": "ERROR",
			"msg":      "test run failed",
			"app":      "tester", // From New(WithAttrs)
			"data": map[string]any{ // From New(WithGroup)
				"run":  float64(1),   // From logger.With
				"code": float64(500), // From logger.Error
			},
		}
		if diff := cmp.Diff(wantPayload, capturedJSON, cmpopts.IgnoreMapEntries(func(k string, v any) bool { return k == "time" })); diff != "" {
			t.Errorf("Stdout log with initial attrs/group mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("WithReplaceAttr", func(t *testing.T) {
		// Set up to capture what happens with the replacer
		var capturedGroups [][]string
		var capturedAttrs []slog.Attr

		// Define a custom replacer that records its inputs
		customReplacer := func(groups []string, a slog.Attr) slog.Attr {
			capturedGroups = append(capturedGroups, groups)
			capturedAttrs = append(capturedAttrs, a)

			// Modify a specific attribute for verification
			if a.Key == slog.LevelKey && len(groups) == 0 {
				return slog.String("test_severity", "TEST")
			}
			return a
		}

		// Use a real stdout capture to verify the replacer's effect
		output := captureOutput(t, &os.Stdout, func() {
			logger, err := New(
				WithLogTarget(LogTargetStdout),
				WithReplaceAttr(customReplacer),
			)
			if err != nil {
				t.Fatalf("New() with custom replacer failed: %v", err)
			}

			// Log something to trigger the replacer
			logger.Info("test message")
		})

		// Verify the replacer was called with the expected parameters
		if len(capturedGroups) == 0 {
			t.Fatal("ReplaceAttr function was never called")
		}

		// Check that at least one call had the level attribute with empty group slice
		levelFound := false
		for i, attr := range capturedAttrs {
			if attr.Key == slog.LevelKey && len(capturedGroups[i]) == 0 {
				levelFound = true
				break
			}
		}
		if !levelFound {
			t.Error("ReplaceAttr was never called with the level attribute and empty groups slice")
		}

		// Verify the output shows our modified attribute
		if !strings.Contains(output, "test_severity") {
			t.Errorf("Expected modified attribute in output, got: %s", output)
		}
	})

}

// TestLogger_CloseFlush verifies Close and Flush behavior.
func TestLogger_CloseFlush(t *testing.T) {
	originalFactory := newClientManagerFunc
	t.Cleanup(func() { newClientManagerFunc = originalFactory })
	mockMgr := newMockClientManager(nil)
	newClientManagerFunc = func(cfg gcp.Config, ua string, co gcp.ClientOptions, lv *slog.LevelVar) clientManagerInterface {
		return mockMgr
	}

	t.Run("GCPMode", func(t *testing.T) {
		mockMgr.Reset()
		logger, err := New(WithProjectID("close-proj"))
		if err != nil {
			t.Fatalf("New() failed: %v", err)
		}
		if logger.clientMgr == nil {
			t.Fatal("clientMgr is nil in GCP mode")
		}

		err = logger.Flush()
		if err != nil {
			t.Errorf("Flush() error = %v, want nil", err)
		}
		if !mockMgr.WasFlushCalled() {
			t.Error("Flush() did not call mockClientManager.Flush()")
		}

		mockMgr.flushCalled = false // Reset flush flag
		err = logger.Close()
		if err != nil {
			t.Errorf("Close() error = %v, want nil", err)
		}
		if !mockMgr.WasFlushCalled() {
			t.Error("Close() did not call mockClientManager.Flush()")
		}
		if !mockMgr.WasCloseCalled() {
			t.Error("Close() did not call mockClientManager.Close()")
		}
	})

	t.Run("StdoutMode", func(t *testing.T) {
		mockMgr.Reset()
		logger, err := New(WithLogTarget(LogTargetStdout))
		if err != nil {
			t.Fatalf("New() failed: %v", err)
		}
		if logger.clientMgr != nil {
			t.Fatal("clientMgr != nil in Stdout mode")
		}

		err = logger.Flush()
		if err != nil {
			t.Errorf("Flush() error = %v in Stdout mode, want nil", err)
		}
		if mockMgr.WasFlushCalled() {
			t.Error("Flush() called mockClientManager.Flush() unexpectedly in Stdout mode")
		}

		err = logger.Close()
		if err != nil {
			t.Errorf("Close() error = %v in Stdout mode, want nil", err)
		}
		if mockMgr.WasCloseCalled() {
			t.Error("Close() called mockClientManager.Close() unexpectedly in Stdout mode")
		}
	})
}

// TestLogger_LevelMethods verifies SetLevel and GetLevel.
func TestLogger_LevelMethods(t *testing.T) {
	originalFactory := newClientManagerFunc
	t.Cleanup(func() { newClientManagerFunc = originalFactory })
	mockMgr := newMockClientManager(nil)
	newClientManagerFunc = func(cfg gcp.Config, ua string, co gcp.ClientOptions, lv *slog.LevelVar) clientManagerInterface {
		return mockMgr
	}

	logger, err := New(WithProjectID("level-proj"), WithLevel(slog.LevelInfo)) // Start at Info
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	if got := logger.GetLevel(); got != slog.LevelInfo {
		t.Errorf("Initial GetLevel() = %v, want %v", got, slog.LevelInfo)
	}
	logger.SetLevel(slog.LevelDebug)
	if got := logger.GetLevel(); got != slog.LevelDebug {
		t.Errorf("GetLevel() after SetLevel(Debug) = %v, want %v", got, slog.LevelDebug)
	}
	logger.SetLevel(LevelNotice.Level())
	if got := logger.GetLevel(); got != LevelNotice.Level() {
		t.Errorf("GetLevel() after SetLevel(Notice) = %v, want %v", got, LevelNotice.Level())
	}
}

// TestLogger_ProjectID verifies the ProjectID method.
func TestLogger_ProjectID(t *testing.T) {
	originalFactory := newClientManagerFunc
	t.Cleanup(func() { newClientManagerFunc = originalFactory })
	mockMgr := newMockClientManager(nil)
	newClientManagerFunc = func(cfg gcp.Config, ua string, co gcp.ClientOptions, lv *slog.LevelVar) clientManagerInterface {
		return mockMgr
	}

	// Case 1: Project ID set via option
	projID1 := "proj-from-option"
	logger1, err := New(WithProjectID(projID1))
	if err != nil {
		t.Fatalf("New(WithProjectID) failed: %v", err)
	}
	if got := logger1.ProjectID(); got != projID1 {
		t.Errorf("ProjectID() with option: got %q, want %q", got, projID1)
	}

	// Case 2: Project ID from environment
	projID2 := "proj-from-env"
	t.Setenv("GOOGLE_CLOUD_PROJECT", projID2)
	logger2, err := New() // No option
	if err != nil {
		t.Fatalf("New() with env var failed: %v", err)
	}
	if got := logger2.ProjectID(); got != projID2 {
		t.Errorf("ProjectID() from env: got %q, want %q", got, projID2)
	}
	t.Setenv("GOOGLE_CLOUD_PROJECT", "") // Clean up

	// Case 3: No project ID (e.g., stdout mode)
	logger3, err := New(WithLogTarget(LogTargetStdout))
	if err != nil {
		t.Fatalf("New(stdout) failed: %v", err)
	}
	if got := logger3.ProjectID(); got != "" {
		t.Errorf("ProjectID() in stdout mode: got %q, want empty string", got)
	}
}

// TestLogger_ConvenienceMethods verifies the GCP-specific level methods delegate correctly.
func TestLogger_ConvenienceMethods(t *testing.T) {
	originalFactory := newClientManagerFunc
	t.Cleanup(func() { newClientManagerFunc = originalFactory })
	mockMgr := newMockClientManager(LevelDefault)
	newClientManagerFunc = func(cfg gcp.Config, ua string, co gcp.ClientOptions, lv *slog.LevelVar) clientManagerInterface {
		return mockMgr
	}

	logger, err := New(WithProjectID("convenience-proj"), WithLevel(LevelDefault.Level())) // Use Default level to ensure Default* methods pass the Enabled check
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	ctx := context.Background()
	testCases := []struct {
		name           string
		logFunc        func() // Function calling the convenience method
		wantLevel      slog.Level
		wantMessage    string
		wantAttrKey    string // Key of the first attribute logged
		shouldHaveAttr bool   // Whether the attribute should be present in payload
	}{
		{"Default", func() { logger.DefaultContext(ctx, "default msg", "k1", 1) }, LevelDefault.Level(), "default msg", "k1", true},
		{"Notice", func() { logger.NoticeContext(ctx, "notice msg", "k2", "v2") }, LevelNotice.Level(), "notice msg", "k2", true},
		{"Critical", func() { logger.CriticalContext(ctx, "critical msg", "k3", true) }, LevelCritical.Level(), "critical msg", "k3", true},
		{"Alert", func() { logger.AlertContext(ctx, "alert msg", "k4", 4.0) }, LevelAlert.Level(), "alert msg", "k4", true},
		{"Emergency", func() { logger.EmergencyContext(ctx, "emergency msg", "k5", nil) }, LevelEmergency.Level(), "emergency msg", "k5", false},
		{"DefaultAttrs", func() { logger.DefaultAttrsContext(ctx, "default attrs", slog.Int("a1", 1)) }, LevelDefault.Level(), "default attrs", "a1", true},
		{"NoticeAttrs", func() { logger.NoticeAttrsContext(ctx, "notice attrs", slog.String("a2", "v2")) }, LevelNotice.Level(), "notice attrs", "a2", true},
		{"CriticalAttrs", func() { logger.CriticalAttrsContext(ctx, "critical attrs", slog.Bool("a3", true)) }, LevelCritical.Level(), "critical attrs", "a3", true},
		{"AlertAttrs", func() { logger.AlertAttrsContext(ctx, "alert attrs", slog.Float64("a4", 4.0)) }, LevelAlert.Level(), "alert attrs", "a4", true},
		{"EmergencyAttrs", func() { logger.EmergencyAttrsContext(ctx, "emergency attrs", slog.Any("a5", nil)) }, LevelEmergency.Level(), "emergency attrs", "a5", false},
	}

	for _, tc := range testCases {
		tc := tc // Capture range variable
		t.Run(tc.name, func(t *testing.T) {
			mockMgr.Reset() // Reset mock for each subtest

			tc.logFunc() // Execute the logging call

			entries := mockMgr.GetCapturedEntries()
			if len(entries) != 1 {
				t.Fatalf("Expected 1 log entry, got %d", len(entries))
			}
			entry := entries[0]

			// Verify Payload Message and Attribute
			payload, ok := entry.Payload.(map[string]any)
			if !ok {
				t.Fatalf("Payload is not map[string]any")
			}
			if msg, _ := payload["message"].(string); msg != tc.wantMessage {
				t.Errorf("Payload message = %q, want %q", msg, tc.wantMessage)
			}

			// Check attribute presence according to expectation
			if tc.shouldHaveAttr {
				if _, ok := payload[tc.wantAttrKey]; !ok {
					t.Errorf("Payload missing expected attribute key %q", tc.wantAttrKey)
				}
			} else {
				if _, ok := payload[tc.wantAttrKey]; ok {
					t.Errorf("Payload should not contain nil-valued attribute key %q", tc.wantAttrKey)
				}
			}
		})
	}
}

// TestLogger_Middleware verifies that middleware functions transform log
// output correctly across configurations and that nil middleware values are
// safely ignored.
func TestLogger_Middleware(t *testing.T) {
	// Preserve the original factory and restore after tests.
	originalFactory := newClientManagerFunc
	t.Cleanup(func() { newClientManagerFunc = originalFactory })

	// attributeMiddleware returns a Middleware that adds key/value to all
	// handled records.
	attributeMiddleware := func(key, value string) Middleware {
		return func(h slog.Handler) slog.Handler {
			return &middlewareTestHandler{
				next: h,
				addAttr: func(r *slog.Record) {
					r.AddAttrs(slog.String(key, value))
				},
			}
		}
	}

	t.Run("StdoutTarget", func(t *testing.T) {
		mw := attributeMiddleware("mw_key", "mw_value")

		output := captureOutput(t, &os.Stdout, func() {
			logger, err := New(
				WithLogTarget(LogTargetStdout),
				WithMiddleware(mw),
			)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			logger.Info("middleware test")
		})

		var parsed map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &parsed); err != nil {
			t.Fatalf("Failed to parse log output: %v\nOutput: %s", err, output)
		}
		if val, ok := parsed["mw_key"]; !ok || val != "mw_value" {
			t.Errorf("Middleware attribute missing or incorrect: %v", parsed)
		}
	})

	t.Run("GCPTarget", func(t *testing.T) {
		mockMgr := newMockClientManager(nil)
		newClientManagerFunc = func(cfg gcp.Config, ua string, co gcp.ClientOptions, lv *slog.LevelVar) clientManagerInterface {
			return mockMgr
		}

		mw := attributeMiddleware("gcp_mw_key", "gcp_mw_value")

		logger, err := New(
			WithProjectID("test-project-id"),
			WithMiddleware(mw),
		)
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		logger.Info("gcp middleware test")

		entries := mockMgr.GetCapturedEntries()
		if len(entries) != 1 {
			t.Fatalf("Expected 1 log entry, got %d", len(entries))
		}

		payload, ok := entries[0].Payload.(map[string]any)
		if !ok {
			t.Fatalf("Entry payload type is %T, want map[string]any", entries[0].Payload)
		}
		if val, ok := payload["gcp_mw_key"].(string); !ok || val != "gcp_mw_value" {
			t.Errorf("Middleware attribute missing or incorrect in GCP entry: %v", payload)
		}
	})

	t.Run("GCPTarget_Fallback", func(t *testing.T) {
		mockMgr := newMockClientManager(nil)
		mockMgr.initErr = errors.New("forced init failure")
		newClientManagerFunc = func(cfg gcp.Config, ua string, co gcp.ClientOptions, lv *slog.LevelVar) clientManagerInterface {
			return mockMgr
		}

		mw := attributeMiddleware("fallback_key", "fallback_value")

		var stdoutOutput string
		_ = captureOutput(t, &os.Stderr, func() { // suppress warnings
			stdoutOutput = captureOutput(t, &os.Stdout, func() {
				logger, err := New(
					WithProjectID("fallback-proj"),
					WithMiddleware(mw),
				)
				if err != nil {
					t.Fatalf("New() error = %v", err)
				}
				logger.Info("fallback test")
			})
		})

		var logData map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(stdoutOutput)), &logData); err != nil {
			t.Fatalf("Failed to parse fallback log JSON: %v\nOutput: %s", err, stdoutOutput)
		}
		if val, ok := logData["fallback_key"]; !ok || val != "fallback_value" {
			t.Errorf("Middleware attribute missing or incorrect in fallback mode: %v", logData)
		}
	})

	t.Run("MultipleMiddleware", func(t *testing.T) {
		mw1 := attributeMiddleware("mw1_key", "mw1_value")
		mw2 := attributeMiddleware("mw2_key", "mw2_value")

		output := captureOutput(t, &os.Stdout, func() {
			logger, err := New(
				WithLogTarget(LogTargetStdout),
				WithMiddleware(mw1),
				WithMiddleware(mw2),
			)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			logger.Info("multiple middleware test")
		})

		var parsed map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &parsed); err != nil {
			t.Fatalf("Failed to parse log output: %v\nOutput: %s", err, output)
		}
		if val, ok := parsed["mw1_key"]; !ok || val != "mw1_value" {
			t.Errorf("First middleware attribute missing or incorrect: %v", parsed)
		}
		if val, ok := parsed["mw2_key"]; !ok || val != "mw2_value" {
			t.Errorf("Second middleware attribute missing or incorrect: %v", parsed)
		}
	})

	t.Run("MultipleMiddleware_WithNilInMiddle", func(t *testing.T) {
		mw1 := attributeMiddleware("mw1_key", "mw1_value")
		var nilMiddleware Middleware // intentionally nil
		mw2 := attributeMiddleware("mw2_key", "mw2_value")

		output := captureOutput(t, &os.Stdout, func() {
			logger, err := New(
				WithLogTarget(LogTargetStdout),
				WithMiddleware(mw1),
				WithMiddleware(nilMiddleware),
				WithMiddleware(mw2),
			)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			logger.Info("nil middleware chain test")
		})

		var parsed map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &parsed); err != nil {
			t.Fatalf("Failed to parse log output: %v\nOutput: %s", err, output)
		}
		if val, ok := parsed["mw1_key"]; !ok || val != "mw1_value" {
			t.Errorf("First middleware attribute missing or incorrect: %v", parsed)
		}
		if val, ok := parsed["mw2_key"]; !ok || val != "mw2_value" {
			t.Errorf("Second middleware attribute missing or incorrect: %v", parsed)
		}
	})
}
