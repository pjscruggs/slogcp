// Copyright 2025-2026 Patrick J. Scruggs
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
	"context"
	"log/slog"
	"testing"

	"github.com/pjscruggs/slogcp"
)

// TestContextWithLoggerStoresAndRetrievesLogger verifies that ContextWithLogger
// stores custom loggers and Logger retrieves overrides and fallbacks correctly.
func TestContextWithLoggerStoresAndRetrievesLogger(t *testing.T) {
	t.Parallel()

	defaultLogger := slog.Default()
	if got := slogcp.Logger(context.Background()); got != defaultLogger {
		t.Fatalf("Logger(context.Background()) = %v, want default logger %v", got, defaultLogger)
	}

	custom := slog.New(slog.DiscardHandler)
	ctx := slogcp.ContextWithLogger(context.Background(), custom)
	if got := slogcp.Logger(ctx); got != custom {
		t.Fatalf("Logger(ctx) = %v, want %v", got, custom)
	}

	overridden := slog.New(slog.DiscardHandler)
	ctx = slogcp.ContextWithLogger(ctx, overridden)
	if got := slogcp.Logger(ctx); got != overridden {
		t.Fatalf("Logger(ctx after override) = %v, want %v", got, overridden)
	}
}

// TestContextWithLoggerHandlesNilInputs ensures helper behavior remains stable when
// callers supply nil contexts or loggers.
func TestContextWithLoggerHandlesNilInputs(t *testing.T) {
	t.Parallel()

	var nilCtx context.Context
	custom := slog.New(slog.DiscardHandler)
	if got := slogcp.ContextWithLogger(nilCtx, custom); got != nilCtx {
		t.Fatalf("ContextWithLogger(nil, custom) = %v, want nil", got)
	}

	ctx := context.Background()
	if got := slogcp.ContextWithLogger(ctx, nil); got != ctx {
		t.Fatalf("ContextWithLogger(ctx, nil) = %v, want original context", got)
	}

	if got := slogcp.Logger(nilCtx); got != slog.Default() {
		t.Fatalf("Logger(nil) = %v, want default logger %v", got, slog.Default())
	}
}

// TestLoggerFromContext verifies presence independently of the global fallback.
func TestLoggerFromContext(t *testing.T) {
	original := slog.Default()
	t.Cleanup(func() { slog.SetDefault(original) })
	first := slog.New(slog.DiscardHandler)
	second := slog.New(slog.DiscardHandler)
	slog.SetDefault(first)
	parent := slogcp.ContextWithLogger(context.Background(), first)
	for _, tc := range []struct {
		name string
		ctx  context.Context
		want *slog.Logger
	}{
		{"nil", nil, nil},
		{"absent", context.Background(), nil},
		{"stored default", parent, first},
		{"inherited", context.WithoutCancel(parent), first},
		{"nil preserves parent", slogcp.ContextWithLogger(parent, nil), first},
		{"shadowed", slogcp.ContextWithLogger(parent, second), second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := slogcp.LoggerFromContext(tc.ctx)
			if got != tc.want || ok != (tc.want != nil) {
				t.Fatalf("lookup = (%v, %v), want %v", got, ok, tc.want)
			}
		})
	}
	slog.SetDefault(second)
	var nilCtx context.Context
	if slogcp.Logger(nilCtx) != second || slogcp.Logger(context.Background()) != second {
		t.Fatal("fallback did not follow global default")
	}
	if got, ok := slogcp.LoggerFromContext(parent); !ok || got != first {
		t.Fatal("stored default changed")
	}
}
