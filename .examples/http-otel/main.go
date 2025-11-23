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

// Command http-otel wires slogcp's HTTP middleware with OpenTelemetry options.
// It demonstrates how to supply a custom tracer provider, propagators, span
// formatter, request filters, and client IP controls.
//
// This example is both documentation, and a test for `slogcp`.
// Our Github workflow tests if any changes to `slogcp` break the example.
package main

import (
	"context"
	"log"
	"log/slog"
	"net/http"
	"os"
	"time"

	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/pjscruggs/slogcp"
	"github.com/pjscruggs/slogcp/slogcphttp"
)

func main() {
	ctx := context.Background()

	tracerProvider := sdktrace.NewTracerProvider()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := tracerProvider.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown tracer provider: %v", err)
		}
	}()

	propagators := propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	)

	handler, err := slogcp.NewHandler(os.Stdout,
		slogcp.WithStackTraceLevel(slog.LevelWarn),
	)
	if err != nil {
		log.Fatalf("failed to create handler: %v", err)
	}
	defer func() {
		if cerr := handler.Close(); cerr != nil {
			log.Printf("handler close: %v", cerr)
		}
	}()

	logger := slog.New(handler)

	middleware := slogcphttp.Middleware(
		slogcphttp.WithLogger(logger),
		slogcphttp.WithTracerProvider(tracerProvider),
		slogcphttp.WithPropagators(propagators),
		slogcphttp.WithPublicEndpoint(true),
		slogcphttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return r.Method + " " + r.URL.Path
		}),
		slogcphttp.WithFilter(func(r *http.Request) bool {
			return r.URL.Path != "/healthz"
		}),
		slogcphttp.WithClientIP(false),
	)

	mux := http.NewServeMux()
	mux.Handle("/widgets/", middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slogcp.Logger(r.Context()).Info("handling request")
		w.WriteHeader(http.StatusNoContent)
	})))
	mux.Handle("/healthz", middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))

	log.Println("http-otel example listening on :8080")
	if err := http.ListenAndServe(":8080", mux); err != nil {
		log.Fatalf("server stopped: %v", err)
	}
}
