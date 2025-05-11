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

	"cloud.google.com/go/logging"
	"google.golang.org/api/option"
)

// mockGcpClientAPI simulates the gcpClientAPI interface for testing ClientManager.
type mockGcpClientAPI struct {
	mu          sync.Mutex
	loggerFn    func(logID string, opts ...logging.LoggerOption) *logging.Logger
	closeFn     func() error
	closeCalled bool
}

func (m *mockGcpClientAPI) Logger(logID string, opts ...logging.LoggerOption) *logging.Logger {
	if m.loggerFn != nil {
		return m.loggerFn(logID, opts...)
	}
	return nil
}

func (m *mockGcpClientAPI) Close() error {
	m.mu.Lock()
	m.closeCalled = true
	m.mu.Unlock()
	if m.closeFn != nil {
		return m.closeFn()
	}
	return nil
}

func (m *mockGcpClientAPI) WasCloseCalled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closeCalled
}

// TestNewClientManager verifies that the constructor sets basic fields correctly.
func TestNewClientManager(t *testing.T) {
	cfg := Config{Parent: "projects/test-project"}
	ua := "test-agent/1.0"
	levelVar := new(slog.LevelVar)

	cm := NewClientManager(cfg, ua, levelVar)

	if cm.cfg.Parent != cfg.Parent {
		t.Errorf("cfg.Parent = %q; want %q", cm.cfg.Parent, cfg.Parent)
	}
	if cm.userAgent != ua {
		t.Errorf("userAgent = %q; want %q", cm.userAgent, ua)
	}
	if cm.levelVar != levelVar {
		t.Error("levelVar mismatch")
	}
	if cm.newClientFn == nil {
		t.Error("newClientFn should be initialized by constructor")
	}
}

// TestClientManager_Initialize covers success and error scenarios.
func TestClientManager_Initialize(t *testing.T) {
	baseCfg := Config{Parent: "projects/init-proj"}
	ua := "init-agent"
	levelVar := new(slog.LevelVar)

	t.Run("Success", func(t *testing.T) {
		mockClient := &mockGcpClientAPI{}
		// Provide a non-nil logger to satisfy creation
		mockClient.loggerFn = func(logID string, opts ...logging.LoggerOption) *logging.Logger {
			return &logging.Logger{}
		}

		factory := func(ctx context.Context, parent string, onError func(error), opts ...option.ClientOption) (gcpClientAPI, error) {
			// Verify parent and UA get passed via opts (UA in option.WithUserAgent)
			if parent != baseCfg.Parent {
				t.Errorf("factory received parent = %q; want %q", parent, baseCfg.Parent)
			}
			return mockClient, nil
		}

		cm := NewClientManager(baseCfg, ua, levelVar)
		cm.newClientFn = factory

		if err := cm.Initialize(); err != nil {
			t.Fatalf("Initialize() = %v; want nil", err)
		}
		// Idempotency
		err2 := cm.Initialize()
		if err2 != nil {
			t.Errorf("Second Initialize() = %v; want nil", err2)
		}
		// Ensure client and logger set
		if cm.client == nil {
			t.Error("client not set after Initialize")
		}
		if cm.logger == nil {
			t.Error("logger not set after Initialize")
		}
	})

	t.Run("FactoryError", func(t *testing.T) {
		errFoo := errors.New("factory fail")
		factory := func(ctx context.Context, parent string, onError func(error), opts ...option.ClientOption) (gcpClientAPI, error) {
			return nil, errFoo
		}

		cm := NewClientManager(baseCfg, ua, levelVar)
		cm.newClientFn = factory

		err := cm.Initialize()
		if !errors.Is(err, errFoo) {
			t.Errorf("Initialize() error = %v; want wrapping %v", err, errFoo)
		}
		if cm.client != nil {
			t.Error("client should be nil on factory error")
		}
		if cm.logger != nil {
			t.Error("logger should be nil on factory error")
		}
	})

	t.Run("LoggerCreationError", func(t *testing.T) {
		mockClient := &mockGcpClientAPI{}
		// Simulate Logger returning nil
		mockClient.loggerFn = func(logID string, opts ...logging.LoggerOption) *logging.Logger {
			return nil
		}
		// Rely on WasCloseCalled for cleanup verification

		factory := func(ctx context.Context, parent string, onError func(error), opts ...option.ClientOption) (gcpClientAPI, error) {
			return mockClient, nil
		}

		cm := NewClientManager(baseCfg, ua, levelVar)
		cm.newClientFn = factory

		err := cm.Initialize()
		if err == nil {
			t.Fatal("Initialize() = nil; want error")
		}
		if !mockClient.WasCloseCalled() {
			t.Error("expected client.Close() on logger failure")
		}
		if cm.client != nil || cm.logger != nil {
			t.Error("client and logger should be nil after logger creation error")
		}
	})
}

// TestClientManager_GetLogger tests retrieving the logger instance.
func TestClientManager_GetLogger(t *testing.T) {
	cfg := Config{Parent: "p"}
	levelVar := new(slog.LevelVar)
	ua := "ua"

	// Before Initialize
	cm := NewClientManager(cfg, ua, levelVar)
	if _, err := cm.GetLogger(); !errors.Is(err, ErrClientNotInitialized) {
		t.Errorf("GetLogger() before init error = %v; want %v", err, ErrClientNotInitialized)
	}

	// After failed Initialize
	cm.initErr = errors.New("fail")
	if _, err := cm.GetLogger(); !errors.Is(err, cm.initErr) {
		t.Errorf("GetLogger() after failed init error = %v; want %v", err, cm.initErr)
	}

	// After successful init
	cm = NewClientManager(cfg, ua, levelVar)
	fakeLogger := &logging.Logger{}
	cm.logger = &RealGcpLogger{Logger: fakeLogger}
	cm.initErr = nil

	got, err := cm.GetLogger()
	if err != nil {
		t.Fatalf("GetLogger() = %v; want nil", err)
	}
	if got.Logger != fakeLogger {
		t.Error("GetLogger() returned unexpected logger instance")
	}
}

// TestClientManager_Close tests the Close logic focusing on client.Close behavior.
func TestClientManager_Close(t *testing.T) {
	cfg := Config{Parent: "p"}
	levelVar := new(slog.LevelVar)
	ua := "ua"

	t.Run("NotInitialized", func(t *testing.T) {
		cm := NewClientManager(cfg, ua, levelVar)
		err := cm.Close()
		if !errors.Is(err, ErrClientNotInitialized) {
			t.Errorf("Close() before init = %v; want %v", err, ErrClientNotInitialized)
		}
	})

	t.Run("InitFailed", func(t *testing.T) {
		cm := NewClientManager(cfg, ua, levelVar)
		failErr := errors.New("init fail")
		cm.initErr = failErr
		err := cm.Close()
		if !errors.Is(err, failErr) {
			t.Errorf("Close() after failed init = %v; want %v", err, failErr)
		}
	})

	t.Run("Success", func(t *testing.T) {
		mockClient := &mockGcpClientAPI{}
		cm := NewClientManager(cfg, ua, levelVar)
		cm.client = mockClient
		cm.initErr = nil

		err := cm.Close()
		if err != nil {
			t.Errorf("Close() = %v; want nil", err)
		}
		if !mockClient.WasCloseCalled() {
			t.Error("expected client.Close() on success")
		}
	})

	t.Run("ClientCloseError", func(t *testing.T) {
		closeErr := errors.New("close failed")
		mockClient := &mockGcpClientAPI{ closeFn: func() error { return closeErr } }
		cm := NewClientManager(cfg, ua, levelVar)
		cm.client = mockClient
		cm.initErr = nil

		err := cm.Close()
		if !errors.Is(err, closeErr) {
			t.Errorf("Close() error = %v; want %v", err, closeErr)
		}
	})

	t.Run("InternalInconsistency_LoggerNil", func(t *testing.T) {
		mockClient := &mockGcpClientAPI{}
		cm := NewClientManager(cfg, ua, levelVar)
		cm.client = mockClient
		cm.logger = nil
		cm.initErr = nil

		err := cm.Close()
		if err == nil || !strings.Contains(err.Error(), "internal inconsistency") {
			t.Errorf("Close() internal inconsistency = %v; want error mentioning inconsistency", err)
		}
		if !mockClient.WasCloseCalled() {
			t.Error("Close() should still call client.Close() on inconsistency")
		}
	})
}

// TestClientManager_GetLeveler tests retrieving the levelVar.
func TestClientManager_GetLeveler(t *testing.T) {
	cfg := Config{Parent: "p"}
	levelVar := new(slog.LevelVar)
	ua := "ua"
	cm := NewClientManager(cfg, ua, levelVar)

	// before init
	if lvl := cm.GetLeveler(); lvl != nil {
		t.Errorf("GetLeveler() before init = %v; want nil", lvl)
	}

	// after failed init
	cm.initErr = errors.New("fail")
	if lvl := cm.GetLeveler(); lvl != nil {
		t.Errorf("GetLeveler() after failed init = %v; want nil", lvl)
	}

	// after success
	cm = NewClientManager(cfg, ua, levelVar)
	cm.logger = &RealGcpLogger{Logger: &logging.Logger{}}
	cm.initErr = nil

	if lvl := cm.GetLeveler(); lvl != levelVar {
		t.Errorf("GetLeveler() = %v; want %v", lvl, levelVar)
	}
}
