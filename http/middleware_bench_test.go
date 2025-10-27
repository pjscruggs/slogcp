package http

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pjscruggs/slogcp"
)

var middlewareBenchStatus int

// BenchmarkMiddlewareBodyCapture measures middleware overhead when capturing bodies with different handlers.
func BenchmarkMiddlewareBodyCapture(b *testing.B) {
	body := bytes.Repeat([]byte("payload-data-"), 4096)
	cases := []struct {
		name      string
		newLogger func(*testing.B) *slog.Logger
	}{
		{
			name: "TextHandler",
			newLogger: func(*testing.B) *slog.Logger {
				return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
			},
		},
		{
			name: "ProductionHandler",
			newLogger: func(b *testing.B) *slog.Logger {
				handler, err := slogcp.NewHandler(
					io.Discard,
					slogcp.WithLevel(slog.LevelDebug),
				)
				if err != nil {
					b.Fatalf("failed to create slogcp handler: %v", err)
				}
				b.Cleanup(func() {
					_ = handler.Close()
				})
				return slog.New(handler)
			},
		},
	}

	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			logger := tc.newLogger(b)

			mw := Middleware(
				logger,
				WithRequestBodyLimit(int64(len(body))),
				WithResponseBodyLimit(int64(len(body))),
				WithLogRequestHeaderKeys("Content-Type", "X-Request-ID"),
				WithLogResponseHeaderKeys("Content-Type"),
				WithRecoverPanics(true),
			)

			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if _, err := io.Copy(w, bytes.NewReader(body)); err != nil {
					b.Fatalf("failed to write response body: %v", err)
				}
			})
			handler := mw(next)

			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				req := httptest.NewRequest(http.MethodPost, "https://example.com/data", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("X-Request-ID", "req-benchmark")

				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				middlewareBenchStatus = rec.Code
			}

			if middlewareBenchStatus == 0 {
				b.Fatal("response code not captured")
			}
		})
	}
}
