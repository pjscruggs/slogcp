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

package slogcphttp

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pjscruggs/slogcp"
)

// BenchmarkMiddlewareServeHTTP measures the middleware path with and without transformers.
func BenchmarkMiddlewareServeHTTP(b *testing.B) {
	handler, err := slogcp.NewHandler(io.Discard)
	if err != nil {
		b.Fatalf("NewHandler returned error: %v", err)
	}
	b.Cleanup(func() { _ = handler.Close() })

	logger := slog.New(handler)

	req := httptest.NewRequest(http.MethodGet, "https://example.com/items?limit=10", nil)
	req.RemoteAddr = "203.0.113.5:1234"
	req.Header.Set("User-Agent", "bench")

	base := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slogcp.Logger(r.Context()).Info("bench", slog.String("token", "secret"))
	})

	transformer := func(attrs []slog.Attr, _ *http.Request, _ *RequestScope) []slog.Attr {
		for i, attr := range attrs {
			if attr.Key == "token" {
				attrs[i].Value = slog.StringValue("[redacted]")
			}
		}
		return attrs
	}

	run := func(b *testing.B, middleware func(http.Handler) http.Handler) {
		b.Helper()
		w := &benchResponseWriter{header: make(http.Header)}
		h := middleware(base)

		b.ReportAllocs()
		b.ResetTimer()

		for b.Loop() {
			h.ServeHTTP(w, req)
		}
	}

	defaultOpts := func() []Option {
		return []Option{
			WithLogger(logger),
			WithProjectID("bench-project"),
			WithOTel(false),
			WithHTTPRequestAttr(true),
		}
	}

	b.Run("NoTransformer", func(b *testing.B) {
		run(b, Middleware(defaultOpts()...))
	})

	b.Run("RedactTransformer", func(b *testing.B) {
		opts := defaultOpts()
		opts = append(opts, WithAttrTransformer(transformer))
		run(b, Middleware(opts...))
	})

	b.Run("NoHTTPRequestAttr", func(b *testing.B) {
		run(b, Middleware(
			WithLogger(logger),
			WithProjectID("bench-project"),
			WithOTel(false),
		))
	})

	b.Run("NoClientIP", func(b *testing.B) {
		opts := defaultOpts()
		opts = append(opts, WithClientIP(false))
		run(b, Middleware(opts...))
	})

	b.Run("NoTracePropagation", func(b *testing.B) {
		opts := defaultOpts()
		opts = append(opts, WithTracePropagation(false))
		run(b, Middleware(opts...))
	})
}

// benchResponseWriter is a minimal ResponseWriter for middleware benchmarks.
type benchResponseWriter struct {
	header http.Header
}

// Header returns the response headers.
func (w *benchResponseWriter) Header() http.Header { return w.header }

// Write discards the body and reports full consumption.
func (w *benchResponseWriter) Write(p []byte) (int, error) { return len(p), nil }

// WriteHeader is a no-op for benchmarking.
func (w *benchResponseWriter) WriteHeader(int) {}
