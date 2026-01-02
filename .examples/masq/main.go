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

// Command masq spins up a small HTTP server that logs requests using slogcp
// while redacting sensitive data with github.com/m-mizutani/masq. It exposes a
// root endpoint that returns a friendly greeting and Cloud Run-style health
// checks at /healthz and /_ah/health.
//
// This example is both documentation, and a test for `slogcp`.
// Our Github workflow tests if any changes to `slogcp` break the example.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/m-mizutani/masq"

	"github.com/pjscruggs/slogcp"
)

const (
	defaultPort  = "8080"
	bearerPrefix = "Bearer "
)

type accessToken string

// newExampleLogger builds a slogcp logger that redacts access tokens before
// emitting structured entries.
func newExampleLogger(w io.Writer) (*slog.Logger, func() error, error) {
	handler, err := slogcp.NewHandler(w,
		slogcp.WithSeverityAliases(false),
		slogcp.WithReplaceAttr(masq.New(
			masq.WithType[accessToken](),
		)),
	)
	if err != nil {
		return nil, nil, err
	}
	return slog.New(handler), handler.Close, nil
}

// newRouter wires up HTTP handlers that log requests with redacted tokens and
// expose health check endpoints.
func newRouter(logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()

	healthHandler := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}

	mux.HandleFunc("/healthz", healthHandler)
	mux.HandleFunc("/_ah/health", healthHandler)

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		token := extractToken(r.Header.Get("Authorization"))
		attrs := []slog.Attr{
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
		}
		if token != "" {
			attrs = append(attrs, slog.Any("token", accessToken(token)))
		}
		logger.LogAttrs(r.Context(), slog.LevelInfo, "hello world", attrs...)

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]string{"message": "hello world"}); err != nil {
			logger.ErrorContext(r.Context(), "write response", slog.Any("error", err))
		}
	})

	return mux
}

// extractToken pulls the bearer token value from an Authorization header.
func extractToken(header string) string {
	if strings.HasPrefix(header, bearerPrefix) {
		return strings.TrimSpace(header[len(bearerPrefix):])
	}
	return ""
}

// run boots the HTTP server, listens for shutdown signals, and ensures a
// graceful stop sequence.
func run(ctx context.Context) error {
	logger, cleanup, err := newExampleLogger(os.Stdout)
	if err != nil {
		return err
	}
	defer func() {
		if cleanup != nil {
			if err := cleanup(); err != nil {
				log.Printf("slogcp handler close: %v", err)
			}
		}
	}()

	port := os.Getenv("PORT")
	if port == "" {
		port = defaultPort
	}

	server := &http.Server{
		Addr:              ":" + port,
		Handler:           newRouter(logger),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	logger.Info("server listening", slog.String("port", port))

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// main starts the server and terminates when the process receives an interrupt
// or SIGTERM.
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
