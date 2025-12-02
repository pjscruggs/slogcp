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

package slogcphttp

import (
	"log/slog"
	"testing"
)

// TestWithLoggerHandlesNil verifies WithLogger falls back to slog.Default().
func TestWithLoggerHandlesNil(t *testing.T) {
	t.Parallel()

	cfg := applyOptions([]Option{WithLogger(nil)})
	if cfg.logger != slog.Default() {
		t.Fatalf("WithLogger(nil) did not restore slog.Default()")
	}

	custom := slog.New(slog.DiscardHandler)
	cfg = applyOptions([]Option{WithLogger(custom)})
	if cfg.logger != custom {
		t.Fatalf("WithLogger custom logger mismatch")
	}
}

// TestWithHTTPRequestAttr toggles automatic httpRequest enrichment flag.
func TestWithHTTPRequestAttr(t *testing.T) {
	t.Parallel()

	cfg := applyOptions([]Option{WithHTTPRequestAttr(true)})
	if !cfg.includeHTTPRequestAttr {
		t.Fatalf("includeHTTPRequestAttr = false, want true")
	}

	cfg = applyOptions([]Option{WithHTTPRequestAttr(false)})
	if cfg.includeHTTPRequestAttr {
		t.Fatalf("includeHTTPRequestAttr = true, want false")
	}
}
