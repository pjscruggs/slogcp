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

package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/pjscruggs/slogcp"
	"github.com/pjscruggs/slogcp-e2e-internal/services/target-apps/core-logging-target-app/handlers"
	"github.com/pjscruggs/slogcp/slogcphttp"
)

// main starts the core logging target app used by e2e tests.
func main() {
	commonAttrs := []slog.Attr{
		slog.Group(slogcp.LabelsGroup,
			slog.String("app", "slogcp-test-target"),
			slog.String("component", "e2e-test"),
		),
	}

	baseOptions := []slogcp.Option{
		slogcp.WithSourceLocationEnabled(true),
		slogcp.WithAttrs(commonAttrs),
	}

	mainHandler, err := slogcp.NewHandler(os.Stdout, append([]slogcp.Option{slogcp.WithLevel(slog.LevelDebug)}, baseOptions...)...)
	if err != nil {
		log.Fatalf("Failed to initialize slogcp handler: %v", err)
	}
	defer func() {
		if err := mainHandler.Close(); err != nil {
			log.Printf("Error closing slog handler: %v", err)
		}
	}()

	defaultHandler, err := slogcp.NewHandler(os.Stdout, append([]slogcp.Option{slogcp.WithLevel(slog.LevelError)}, baseOptions...)...)
	if err != nil {
		log.Printf("Failed to initialize slogcp default severity handler: %v", err)
		return
	}
	defer func() {
		if err := defaultHandler.Close(); err != nil {
			log.Printf("Error closing slog default severity handler: %v", err)
		}
	}()

	logger := slog.New(mainHandler)
	defaultLogger := slog.New(defaultHandler)

	// Log startup
	logger.InfoContext(context.Background(), "Target app starting",
		"version", getEnvOrDefault("APP_VERSION", "dev"),
		"port", getEnvOrDefault("PORT", "8080"),
	)

	// Check if we can use the DEFAULT severity with slogcp
	slogcp.DefaultContext(context.Background(), defaultLogger, "This is a slogcp log with DEFAULT severity",
		"source", "slogcp-test",
		"version", getEnvOrDefault("APP_VERSION", "dev"),
	)

	// Create handlers with the logger
	coreLoggingHandler := handlers.NewCoreLoggingHandler(logger, defaultLogger)

	// Set up routes
	mux := http.NewServeMux()

	// Health check endpoint (minimal logging)
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if _, err := fmt.Fprint(w, `{"status":"healthy"}`); err != nil {
			logger.ErrorContext(context.Background(), "Failed to write health response", "error", err)
		}
	})

	// Core logging test endpoints
	mux.HandleFunc("/log/default", coreLoggingHandler.LogDefault)
	mux.HandleFunc("/log/debug", coreLoggingHandler.LogDebug)
	mux.HandleFunc("/log/info", coreLoggingHandler.LogInfo)
	mux.HandleFunc("/log/notice", coreLoggingHandler.LogNotice)
	mux.HandleFunc("/log/warning", coreLoggingHandler.LogWarning)
	mux.HandleFunc("/log/error", coreLoggingHandler.LogError)
	mux.HandleFunc("/log/critical", coreLoggingHandler.LogCritical)
	mux.HandleFunc("/log/alert", coreLoggingHandler.LogAlert)
	mux.HandleFunc("/log/emergency", coreLoggingHandler.LogEmergency)

	// Structured logging endpoints
	mux.HandleFunc("/log/structured", coreLoggingHandler.LogStructured)
	mux.HandleFunc("/log/nested", coreLoggingHandler.LogNested)
	mux.HandleFunc("/log/operation", coreLoggingHandler.LogWithOperation)
	mux.HandleFunc("/log/labels", coreLoggingHandler.LogWithLabels)
	mux.HandleFunc("/log/http-request", coreLoggingHandler.LogHTTPRequest)
	mux.HandleFunc("/log/http-request-inflight", coreLoggingHandler.LogHTTPRequestInflight)
	mux.HandleFunc("/log/http-request-parser-residual", coreLoggingHandler.LogHTTPRequestParserResidual)
	mux.HandleFunc("/log/http-request-parser-status-zero", coreLoggingHandler.LogHTTPRequestParserStatusZero)

	// Batch logging endpoint for testing multiple logs
	mux.HandleFunc("/log/batch", coreLoggingHandler.LogBatch)

	// Middleware exercise endpoints
	mux.HandleFunc("/middleware/echo", coreLoggingHandler.MiddlewareEcho)
	mux.HandleFunc("/middleware/panic", coreLoggingHandler.MiddlewarePanic)
	mux.HandleFunc("/middleware/skip-logging", coreLoggingHandler.MiddlewareSkip)

	// Apply slogcp HTTP middleware for request logging only
	wrappedHandler := slogcphttp.Middleware(
		slogcphttp.WithLogger(logger),
	)(mux)

	// Create HTTP server
	port := getEnvOrDefault("PORT", "8080")
	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      wrappedHandler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// Start server in a goroutine
	go func() {
		logger.InfoContext(context.Background(), "HTTP server starting", "port", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.ErrorContext(context.Background(), "HTTP server error", "error", err)
			os.Exit(1)
		}
	}()

	// Wait for interrupt signal to gracefully shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigChan

	logger.InfoContext(context.Background(), "Shutdown signal received", "signal", sig)

	// Graceful shutdown with timeout
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.ErrorContext(shutdownCtx, "Server shutdown error", "error", err)
	}

	logger.InfoContext(context.Background(), "Target app stopped")
}

// getEnvOrDefault returns key when present, otherwise defaultValue.
func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
