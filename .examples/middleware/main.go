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

// Command middleware demonstrates composing slogcp with a custom middleware that
// redacts secrets while also enabling stack traces for elevated log levels.
//
// This example is both documentation, and a test for `slogcp`.
// Our Github workflow tests if any changes to `slogcp` break the example.
package main

import (
	"context"
	"log"
	"log/slog"

	"github.com/pjscruggs/slogcp"
)

// main constructs a slogcp handler that redirects to stdout, adds middleware
// driven attribute scrubbing, and captures stack traces for warnings or higher.
func main() {
	handler, err := slogcp.NewHandler(nil,
		slogcp.WithRedirectToStdout(),
		slogcp.WithStackTraceEnabled(true),
		slogcp.WithStackTraceLevel(slog.LevelWarn),
		slogcp.WithMiddleware(redactSecretsMiddleware),
	)
	if err != nil {
		log.Fatalf("failed to create slogcp handler: %v", err)
	}
	defer handler.Close()

	logger := slog.New(handler)
	logger.Info("issuing request",
		slog.String("api_key", "secret-123"),
		slog.String("user", "tulip"),
	)
	logger.Warn("slow response encountered", slog.String("trace_id", "abc123"))
}

// secretScrubber wraps a slog.Handler to prevent accidental emission of tokens.
type secretScrubber struct {
	next slog.Handler
}

// Enabled delegates level checks to the wrapped handler.
func (s secretScrubber) Enabled(ctx context.Context, level slog.Level) bool {
	return s.next.Enabled(ctx, level)
}

// Handle clones the record, redacting known sensitive attributes before
// forwarding it to the wrapped handler.
func (s secretScrubber) Handle(ctx context.Context, r slog.Record) error {
	clean := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(attr slog.Attr) bool {
		if attr.Key == "api_key" || attr.Key == "token" {
			attr.Value = slog.StringValue("[redacted]")
		}
		clean.AddAttrs(attr)
		return true
	})
	return s.next.Handle(ctx, clean)
}

// WithAttrs propagates attr scoping through the middleware boundary.
func (s secretScrubber) WithAttrs(attrs []slog.Attr) slog.Handler {
	return secretScrubber{next: s.next.WithAttrs(attrs)}
}

// WithGroup propagates group scoping through the middleware boundary.
func (s secretScrubber) WithGroup(name string) slog.Handler {
	return secretScrubber{next: s.next.WithGroup(name)}
}

// redactSecretsMiddleware returns a slogcp middleware that masks sensitive
// attributes before emitting records.
func redactSecretsMiddleware(next slog.Handler) slog.Handler {
	return secretScrubber{next: next}
}
