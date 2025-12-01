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
	"io"
	"path/filepath"
	"testing"

	"github.com/pjscruggs/slogcp/slogcpasync"
)

// TestWithAsyncWrapsHandler ensures WithAsync applies the async wrapper.
func TestWithAsyncWrapsHandler(t *testing.T) {
	h, err := NewHandler(io.Discard, WithAsync())
	if err != nil {
		t.Fatalf("NewHandler returned %v", err)
	}
	if _, ok := h.Handler.(*slogcpasync.Handler); !ok {
		t.Fatalf("Handler is %T, want *slogcpasync.Handler", h.Handler)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close returned %v", err)
	}
}

// TestWithAsyncOnFileTargetsWrapsOnlyFiles applies async to file targets only.
func TestWithAsyncOnFileTargetsWrapsOnlyFiles(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "log.json")

	fileHandler, err := NewHandler(nil, WithRedirectToFile(logPath), WithAsyncOnFileTargets())
	if err != nil {
		t.Fatalf("file NewHandler returned %v", err)
	}
	if _, ok := fileHandler.Handler.(*slogcpasync.Handler); !ok {
		t.Fatalf("file Handler is %T, want *slogcpasync.Handler", fileHandler.Handler)
	}
	if err := fileHandler.Close(); err != nil {
		t.Fatalf("file Close returned %v", err)
	}

	stdHandler, err := NewHandler(io.Discard, WithAsyncOnFileTargets())
	if err != nil {
		t.Fatalf("stdout NewHandler returned %v", err)
	}
	if _, ok := stdHandler.Handler.(*slogcpasync.Handler); ok {
		t.Fatalf("stdout Handler unexpectedly async-wrapped")
	}
	if err := stdHandler.Close(); err != nil {
		t.Fatalf("stdout Close returned %v", err)
	}
}
