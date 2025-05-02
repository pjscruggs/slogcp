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

package gcp

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/logging"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/api/option"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
)

// mockGcpClientAPI simulates the gcpClientAPI interface for testing ClientManager.
type mockGcpClientAPI struct {
	mu          sync.Mutex
	loggerFn    func(logID string, opts ...logging.LoggerOption) *logging.Logger
	closeFn     func() error
	closeCalled bool
	// Note: OnError cannot be easily mocked/verified without a real client instance.
}

func (m *mockGcpClientAPI) Logger(logID string, opts ...logging.LoggerOption) *logging.Logger {
	if m.loggerFn != nil {
		return m.loggerFn(logID, opts...)
	}
	// Default behavior: return nil. Tests needing a logger must provide loggerFn.
	return nil
}

func (m *mockGcpClientAPI) Close() error {
	m.mu.Lock()
	m.closeCalled = true
	m.mu.Unlock()
	if m.closeFn != nil {
		return m.closeFn()
	}
	return nil // Default: successful close
}

func (m *mockGcpClientAPI) WasCloseCalled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closeCalled
}

// Compile-time check that mockGcpClientAPI implements gcpClientAPI.
var _ gcpClientAPI = (*mockGcpClientAPI)(nil)

// mockGcpLoggerAPI simulates the gcpLoggerAPI interface for testing ClientManager.
type mockGcpLoggerAPI struct {
	mu          sync.Mutex
	logFn       func(logging.Entry)
	flushFn     func() error
	logCalled   bool
	flushCalled bool
	lastEntry   logging.Entry
}

func (m *mockGcpLoggerAPI) Log(e logging.Entry) {
	m.mu.Lock()
	m.logCalled = true
	m.lastEntry = e // Capture the last entry logged
	m.mu.Unlock()
	if m.logFn != nil {
		m.logFn(e)
	}
}

func (m *mockGcpLoggerAPI) Flush() error {
	m.mu.Lock()
	m.flushCalled = true
	m.mu.Unlock()
	if m.flushFn != nil {
		return m.flushFn()
	}
	return nil // Default: successful flush
}

func (m *mockGcpLoggerAPI) WasLogCalled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.logCalled
}

func (m *mockGcpLoggerAPI) WasFlushCalled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.flushCalled
}

func (m *mockGcpLoggerAPI) GetLastEntry() logging.Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastEntry
}

// Compile-time check that mockGcpLoggerAPI implements gcpLoggerAPI.
var _ GcpLoggerAPI = (*mockGcpLoggerAPI)(nil)

// TestNewClientManager verifies the constructor sets fields correctly.
func TestNewClientManager(t *testing.T) {
	cfg := Config{ProjectID: "test-proj"}
	userAgent := "test-agent/1.0"
	opts := ClientOptions{EntryCountThreshold: pint(100)}
	levelVar := new(slog.LevelVar)

	cm := NewClientManager(cfg, userAgent, opts, levelVar)

	if cm.cfg.ProjectID != cfg.ProjectID {
		t.Errorf("NewClientManager() cfg.ProjectID = %q, want %q", cm.cfg.ProjectID, cfg.ProjectID)
	}
	if cm.userAgent != userAgent {
		t.Errorf("NewClientManager() userAgent = %q, want %q", cm.userAgent, userAgent)
	}
	if cm.clientOpts.EntryCountThreshold == nil || *cm.clientOpts.EntryCountThreshold != 100 {
		t.Errorf("NewClientManager() clientOpts.EntryCountThreshold = %v, want pointer to 100", cm.clientOpts.EntryCountThreshold)
	}
	if cm.levelVar != levelVar {
		t.Error("NewClientManager() levelVar mismatch")
	}
	if cm.newClientFn == nil {
		t.Error("NewClientManager() newClientFn should not be nil")
	}
}

// TestClientManager_Initialize verifies the initialization logic.
func TestClientManager_Initialize(t *testing.T) {
	baseCfg := Config{ProjectID: "init-proj"}
	levelVar := new(slog.LevelVar)
	userAgent := "init-agent"

	t.Run("Success", func(t *testing.T) {
		mockClient := &mockGcpClientAPI{}
		// Mock the logger creation within the client mock
		mockClient.loggerFn = func(logID string, opts ...logging.LoggerOption) *logging.Logger {
			// Return a minimal non-nil logger to satisfy the check in Initialize.
			// This uses a simplified approach; a real logger isn't created.
			return &logging.Logger{} // Placeholder non-nil logger
		}

		factory := func(ctx context.Context, projectID string, opts ...option.ClientOption) (gcpClientAPI, error) {
			// Basic check for user agent propagation
			// This is a weak check as we can't inspect the option internals easily.
			if len(opts) == 0 {
				t.Error("Expected client options (like user agent), but got none")
			}
			return mockClient, nil
		}

		cm := NewClientManager(baseCfg, userAgent, ClientOptions{}, levelVar)
		cm.newClientFn = factory // Inject mock factory

		err := cm.Initialize()
		if err != nil {
			t.Fatalf("Initialize() failed unexpectedly: %v", err)
		}

		// Verify internal state
		if cm.initErr != nil {
			t.Errorf("Initialize() initErr = %v, want nil", cm.initErr)
		}
		if cm.client == nil {
			t.Error("Initialize() client should be set on success")
		}
		if cm.logger == nil {
			t.Error("Initialize() logger should be set on success")
		}

		// Verify idempotency
		err2 := cm.Initialize()
		if err2 != nil {
			t.Errorf("Second Initialize() call failed: %v", err2)
		}
		if cm.client != mockClient { // Check instance remains the same
			t.Error("Second Initialize() call changed the client instance")
		}
	})

	t.Run("ClientFactoryError", func(t *testing.T) {
		factoryErr := errors.New("mock factory error")
		factory := func(ctx context.Context, projectID string, opts ...option.ClientOption) (gcpClientAPI, error) {
			// Return the raw error WITHOUT wrapping it with ErrClientInitializationFailed
			return nil, factoryErr
		}

		cm := NewClientManager(baseCfg, userAgent, ClientOptions{}, levelVar)
		cm.newClientFn = factory // Inject mock factory

		err := cm.Initialize()
		if err == nil {
			t.Fatal("Initialize() did not return expected error")
		}
		// Check that the returned error contains the original factory error
		if !errors.Is(err, factoryErr) {
			t.Errorf("Initialize() error = %v, want wrapping %v", err, factoryErr)
		}

		// Verify internal state
		if cm.initErr == nil || !errors.Is(cm.initErr, factoryErr) {
			t.Errorf("Initialize() initErr = %v, want wrapping %v", cm.initErr, factoryErr)
		}
		if cm.client != nil {
			t.Error("Initialize() client should be nil on factory error")
		}
		if cm.logger != nil {
			t.Error("Initialize() logger should be nil on factory error")
		}

		// Verify idempotency (should still return the original error)
		err2 := cm.Initialize()
		if !errors.Is(err2, factoryErr) {
			t.Errorf("Second Initialize() call error mismatch: got %v, want %v", err2, factoryErr)
		}
	})

	t.Run("LoggerCreationError", func(t *testing.T) {
		mockClient := &mockGcpClientAPI{}
		// Mock loggerFn to return nil, simulating logger creation failure
		mockClient.loggerFn = func(logID string, opts ...logging.LoggerOption) *logging.Logger {
			return nil
		}
		closeCalled := false
		mockClient.closeFn = func() error { // Track if Close is called during cleanup
			closeCalled = true
			return nil
		}

		factory := func(ctx context.Context, projectID string, opts ...option.ClientOption) (gcpClientAPI, error) {
			return mockClient, nil
		}

		cm := NewClientManager(baseCfg, userAgent, ClientOptions{}, levelVar)
		cm.newClientFn = factory // Inject mock factory

		err := cm.Initialize()
		if err == nil {
			t.Fatal("Initialize() did not return expected error on logger failure")
		}
		if !errors.Is(err, ErrClientInitializationFailed) { // Should still wrap the base error type
			t.Errorf("Initialize() error = %v, want wrapping %v", err, ErrClientInitializationFailed)
		}
		if !strings.Contains(err.Error(), "client.Logger(") {
			t.Errorf("Initialize() error message %q does not mention logger failure", err.Error())
		}

		// Verify internal state
		if cm.initErr == nil || !errors.Is(cm.initErr, ErrClientInitializationFailed) {
			t.Errorf("Initialize() initErr = %v, want wrapping %v", cm.initErr, ErrClientInitializationFailed)
		}
		// Client might exist briefly but should be nil after cleanup attempt
		if cm.client != nil {
			t.Error("Initialize() client should be nil after logger creation failure and cleanup")
		}
		if cm.logger != nil {
			t.Error("Initialize() logger should be nil on logger creation failure")
		}
		if !closeCalled {
			t.Error("Expected client.Close() to be called during cleanup after logger failure")
		}
	})

	t.Run("WithOptions", func(t *testing.T) {
		mockClient := &mockGcpClientAPI{}
		var capturedLoggerOpts []logging.LoggerOption // Capture options passed to Logger()

		mockClient.loggerFn = func(logID string, opts ...logging.LoggerOption) *logging.Logger {
			capturedLoggerOpts = opts // Capture the passed options
			return &logging.Logger{}  // Return non-nil logger
		}

		factory := func(ctx context.Context, projectID string, opts ...option.ClientOption) (gcpClientAPI, error) {
			return mockClient, nil
		}

		// Define client options to test
		clientOpts := ClientOptions{
			EntryCountThreshold: pint(50),
			DelayThreshold:      durationPtr(5 * time.Second),
			MonitoredResource:   &mrpb.MonitoredResource{Type: "test_resource"},
		}

		cm := NewClientManager(baseCfg, userAgent, clientOpts, levelVar)
		cm.newClientFn = factory

		err := cm.Initialize()
		if err != nil {
			t.Fatalf("Initialize() failed: %v", err)
		}

		// Verify that the options were passed. This is a basic check on the number
		// of options, as inspecting the function types themselves is complex.
		// Expected options: BufferedByteLimit + EntryCountThreshold + DelayThreshold + CommonResource
		expectedOptCount := 4
		if len(capturedLoggerOpts) != expectedOptCount {
			t.Errorf("Initialize() expected %d logger options, got %d", expectedOptCount, len(capturedLoggerOpts))
		}
	})
}

// TestClientManager_GetLogger tests retrieving the logger instance.
func TestClientManager_GetLogger(t *testing.T) {
	levelVar := new(slog.LevelVar)
	cm := NewClientManager(Config{}, "", ClientOptions{}, levelVar)

	// Case 1: Before Initialize
	_, err := cm.GetLogger()
	if !errors.Is(err, ErrClientNotInitialized) {
		t.Errorf("GetLogger() before Initialize: got error %v, want %v", err, ErrClientNotInitialized)
	}

	// Case 2: After failed Initialize
	initFailErr := errors.New("init failed")
	cm.initErr = initFailErr // Simulate failed init
	_, err = cm.GetLogger()
	if !errors.Is(err, initFailErr) {
		t.Errorf("GetLogger() after failed Initialize: got error %v, want %v", err, initFailErr)
	}

	// Case 3: After successful Initialize
	cm = NewClientManager(Config{}, "", ClientOptions{}, levelVar) // Reset manager
	mockLogger := &mockGcpLoggerAPI{}
	cm.logger = mockLogger // Simulate successful init by setting logger directly
	cm.initErr = nil       // Ensure initErr is nil

	gotLogger, err := cm.GetLogger()
	if err != nil {
		t.Fatalf("GetLogger() after successful Initialize failed: %v", err)
	}
	if gotLogger != mockLogger {
		t.Error("GetLogger() did not return the expected logger instance")
	}
}

// TestClientManager_Log tests the Log method delegation.
func TestClientManager_Log(t *testing.T) {
	levelVar := new(slog.LevelVar)
	entry := logging.Entry{Payload: "test payload"}

	t.Run("NotInitialized", func(t *testing.T) {
		cm := NewClientManager(Config{}, "", ClientOptions{}, levelVar)
		// We check that it doesn't panic and returns nil.
		// Verifying stderr requires more complex test setup.
		err := cm.Log(entry)
		if err != nil {
			t.Errorf("Log() before Initialize returned error %v, want nil", err)
		}
	})

	t.Run("InitFailed", func(t *testing.T) {
		cm := NewClientManager(Config{}, "", ClientOptions{}, levelVar)
		cm.initErr = errors.New("simulated init failure")
		err := cm.Log(entry)
		if err != nil {
			t.Errorf("Log() after failed Initialize returned error %v, want nil", err)
		}
	})

	t.Run("Success", func(t *testing.T) {
		cm := NewClientManager(Config{}, "", ClientOptions{}, levelVar)
		mockLogger := &mockGcpLoggerAPI{}
		cm.logger = mockLogger // Simulate successful init
		cm.initErr = nil

		err := cm.Log(entry)
		if err != nil {
			t.Errorf("Log() after successful Initialize returned error %v, want nil", err)
		}

		if !mockLogger.WasLogCalled() {
			t.Error("Expected mockLogger.Log() to be called")
		}
		// Use cmp.Diff for comparing the captured entry.
		if diff := cmp.Diff(entry, mockLogger.GetLastEntry()); diff != "" {
			t.Errorf("Logged entry mismatch (-want +got):\n%s", diff)
		}
	})
}

// TestClientManager_Close tests the Close method logic.
func TestClientManager_Close(t *testing.T) {
	levelVar := new(slog.LevelVar)

	t.Run("NotInitialized", func(t *testing.T) {
		cm := NewClientManager(Config{}, "", ClientOptions{}, levelVar)
		err := cm.Close()
		if !errors.Is(err, ErrClientNotInitialized) {
			t.Errorf("Close() before Initialize: got error %v, want %v", err, ErrClientNotInitialized)
		}
	})

	t.Run("InitFailed", func(t *testing.T) {
		cm := NewClientManager(Config{}, "", ClientOptions{}, levelVar)
		initFailErr := errors.New("simulated init failure")
		cm.initErr = initFailErr // Simulate failed init
		err := cm.Close()
		if !errors.Is(err, initFailErr) {
			t.Errorf("Close() after failed Initialize: got error %v, want %v", err, initFailErr)
		}
	})

	t.Run("Success", func(t *testing.T) {
		mockClient := &mockGcpClientAPI{}
		mockLogger := &mockGcpLoggerAPI{}
		cm := NewClientManager(Config{}, "", ClientOptions{}, levelVar)
		cm.client = mockClient // Simulate successful init
		cm.logger = mockLogger
		cm.initErr = nil

		err := cm.Close()
		if err != nil {
			t.Fatalf("Close() failed unexpectedly: %v", err)
		}

		if !mockLogger.WasFlushCalled() {
			t.Error("Expected mockLogger.Flush() to be called")
		}
		if !mockClient.WasCloseCalled() {
			t.Error("Expected mockClient.Close() to be called")
		}

		// Test idempotency
		mockLogger.flushCalled = false // Reset flags
		mockClient.closeCalled = false
		err2 := cm.Close()
		if err2 != nil {
			t.Errorf("Second Close() call returned error %v, want nil", err2)
		}
		if mockLogger.WasFlushCalled() {
			t.Error("Second Close() call unexpectedly called Flush again")
		}
		if mockClient.WasCloseCalled() {
			t.Error("Second Close() call unexpectedly called Close again")
		}
	})

	t.Run("FlushError", func(t *testing.T) {
		mockClient := &mockGcpClientAPI{}
		mockLogger := &mockGcpLoggerAPI{}
		flushErr := errors.New("flush failed")
		mockLogger.flushFn = func() error { return flushErr } // Simulate flush error

		cm := NewClientManager(Config{}, "", ClientOptions{}, levelVar)
		cm.client = mockClient
		cm.logger = mockLogger
		cm.initErr = nil

		err := cm.Close()
		if !errors.Is(err, flushErr) {
			t.Errorf("Close() with flush error: got %v, want %v", err, flushErr)
		}
		if !mockClient.WasCloseCalled() { // Close should still be called
			t.Error("Expected mockClient.Close() to be called even if Flush failed")
		}
	})

	t.Run("ClientCloseError", func(t *testing.T) {
		mockClient := &mockGcpClientAPI{}
		mockLogger := &mockGcpLoggerAPI{}
		closeErr := errors.New("client close failed")
		mockClient.closeFn = func() error { return closeErr } // Simulate client close error

		cm := NewClientManager(Config{}, "", ClientOptions{}, levelVar)
		cm.client = mockClient
		cm.logger = mockLogger
		cm.initErr = nil

		err := cm.Close()
		if !errors.Is(err, closeErr) {
			t.Errorf("Close() with client close error: got %v, want %v", err, closeErr)
		}
		if !mockLogger.WasFlushCalled() { // Flush should still be called first
			t.Error("Expected mockLogger.Flush() to be called before failed Close")
		}
	})

	t.Run("FlushAndClientCloseError", func(t *testing.T) {
		mockClient := &mockGcpClientAPI{}
		mockLogger := &mockGcpLoggerAPI{}
		flushErr := errors.New("flush failed")
		closeErr := errors.New("client close failed")
		mockLogger.flushFn = func() error { return flushErr }
		mockClient.closeFn = func() error { return closeErr }

		cm := NewClientManager(Config{}, "", ClientOptions{}, levelVar)
		cm.client = mockClient
		cm.logger = mockLogger
		cm.initErr = nil

		err := cm.Close()
		// Client close error should take precedence
		if !errors.Is(err, closeErr) {
			t.Errorf("Close() with both errors: got %v, want client close error %v", err, closeErr)
		}
	})

	t.Run("InternalInconsistency_LoggerNil", func(t *testing.T) {
		mockClient := &mockGcpClientAPI{} // Client exists
		cm := NewClientManager(Config{}, "", ClientOptions{}, levelVar)
		cm.client = mockClient
		cm.logger = nil // But logger is nil (simulates partial init failure)
		cm.initErr = nil

		err := cm.Close()
		if err == nil {
			t.Error("Close() with logger=nil unexpectedly returned nil error")
		} else if !strings.Contains(err.Error(), "internal inconsistency") {
			t.Errorf("Close() with logger=nil returned wrong error: %v", err)
		}
		if !mockClient.WasCloseCalled() { // Should still try to close client
			t.Error("Expected mockClient.Close() to be called when logger was nil")
		}
	})
}

// TestClientManager_GetLeveler tests retrieving the leveler.
func TestClientManager_GetLeveler(t *testing.T) {
	levelVar := new(slog.LevelVar)
	cm := NewClientManager(Config{}, "", ClientOptions{}, levelVar)

	// Case 1: Before Initialize
	if lvl := cm.GetLeveler(); lvl != nil {
		t.Errorf("GetLeveler() before Initialize: got %v, expected nil", lvl)
	}

	// Case 2: After failed Initialize
	cm.initErr = errors.New("init failed") // Simulate failed init
	if lvl := cm.GetLeveler(); lvl != nil {
		t.Errorf("GetLeveler() after failed Initialize: got %v, expected nil", lvl)
	}

	// Case 3: After successful Initialize
	cm = NewClientManager(Config{}, "", ClientOptions{}, levelVar) // Reset manager
	cm.logger = &mockGcpLoggerAPI{}                                // Simulate successful init
	cm.initErr = nil

	gotLeveler := cm.GetLeveler()
	if gotLeveler == nil {
		t.Fatal("GetLeveler() after successful Initialize returned nil")
	}
	if gotLeveler != levelVar {
		t.Error("GetLeveler() did not return the expected levelVar instance")
	}
}

// pint returns a pointer to an int.
func pint(i int) *int {
	return &i
}

// durationPtr returns a pointer to a time.Duration.
func durationPtr(d time.Duration) *time.Duration {
	return &d
}
