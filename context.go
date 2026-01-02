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

package slogcp

import (
	"context"
	"log/slog"
)

type contextKey int

const (
	loggerContextKey contextKey = iota
)

// ContextWithLogger returns a child context that stores logger so handlers can
// retrieve a request-scoped logger later in the call chain.
func ContextWithLogger(ctx context.Context, logger *slog.Logger) context.Context {
	if ctx == nil || logger == nil {
		return ctx
	}
	return context.WithValue(ctx, loggerContextKey, logger)
}

// Logger retrieves a logger stored in ctx via ContextWithLogger. If no logger
// is found, slog.Default() is returned to ensure callers always receive a
// usable logger.
func Logger(ctx context.Context) *slog.Logger {
	if ctx == nil {
		return slog.Default()
	}
	if logger, ok := ctx.Value(loggerContextKey).(*slog.Logger); ok && logger != nil {
		return logger
	}
	return slog.Default()
}
