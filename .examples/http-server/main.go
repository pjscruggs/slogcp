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

// Command http-server exposes an instrumented HTTP handler that derives trace
// context and logs requests using slogcp middleware components.
//
// This example is both documentation, and a test for `slogcp`.
// Our Github workflow tests if any changes to `slogcp` break the example.
package main

import (
	"log"
	"log/slog"
	"net/http"
	"os"

	"github.com/pjscruggs/slogcp"
	"github.com/pjscruggs/slogcp/slogcphttp"
)

// main starts the HTTP server example with slogcp logging middleware.
func main() {
	handler, err := slogcp.NewHandler(os.Stdout)
	if err != nil {
		log.Fatalf("failed to create slogcp handler: %v", err)
	}
	defer handler.Close()

	logger := slog.New(handler)

	wrapped := slogcphttp.Middleware(
		slogcphttp.WithLogger(logger),
	)(
		slogcphttp.InjectTraceContextMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			logger.InfoContext(r.Context(), "handling request")
			w.WriteHeader(http.StatusNoContent)
		})),
	)

	http.Handle("/api", wrapped)
	if err := http.ListenAndServe(":8080", nil); err != nil {
		logger.Error("server stopped", slog.String("error", err.Error()))
	}
}
