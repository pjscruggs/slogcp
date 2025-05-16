//go:build smoke
// +build smoke

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

package slogcp_test

import (
	"log/slog"
	"testing"

	"github.com/pjscruggs/slogcp"
)

// newTestLogger creates a local‑redirect slogcp.Logger for use in smoke tests.
func newTestLogger(t *testing.T) *slogcp.Logger {
	t.Helper()

	lg, err := slogcp.New(
		slogcp.WithRedirectToStdout(),
		slogcp.WithLevel(slog.LevelInfo),
	)
	if err != nil {
		t.Fatalf("slogcp.New() returned %v, want nil", err)
	}
	return lg
}

// TestLoggerSmoke verifies that New succeeds, Info does not panic, and Close is
// idempotent (two calls return nil).
func TestLoggerSmoke(t *testing.T) {
	t.Parallel()

	lg := newTestLogger(t)

	lg.Info("smoke‑test message")

	if err := lg.Close(); err != nil {
		t.Errorf("Logger.Close() first call returned %v, want nil", err)
	}
	if err := lg.Close(); err != nil {
		t.Errorf("Logger.Close() second call returned %v, want nil", err)
	}
}
