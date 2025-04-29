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

// Package http provides standard net/http middleware for integrating slogcp
// logging with HTTP servers.
//
// The primary offering is the [Middleware] function. It wraps an existing
// [http.Handler] to automatically log details about each incoming request and
// its response using a provided [slog.Logger] (obtained from the main slogcp
// package).
//
// This middleware extracts trace context from request headers via OpenTelemetry
// propagators and includes request/response details (like status code, size,
// latency, remote IP) in the log record. These details are structured to
// populate the dedicated httpRequest field in Cloud Logging entries when used
// with the slogcp handler, enabling better filtering and analysis in the GCP console.
//
// # Basic Usage
//
// First, obtain a logger instance from the main slogcp package:
//
//	// Assumes GOOGLE_CLOUD_PROJECT is set or running on GCP.
//	slogcpLogger, err := slogcp.New()
//	if err != nil {
//	    log.Fatalf("Failed to create slogcp logger: %v", err)
//	}
//	defer slogcpLogger.Close()
//
// Then, apply the middleware to your HTTP handler:
//
//	// Import the middleware package
//	import slogcphttp "github.com/pjscruggs/slogcp/http"
//
//	myHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
//	    // Your handler logic...
//	    w.WriteHeader(http.StatusOK)
//	    w.Write([]byte("Hello, world!"))
//	})
//
//	// Wrap the handler with the middleware.
//	// Note: Middleware expects *slog.Logger, which slogcp.Logger embeds.
//	loggedHandler := slogcphttp.Middleware(slogcpLogger.Logger)(myHandler)
//
//	// Start the server with the wrapped handler.
//	log.Println("Starting server on :8080")
//	if err := http.ListenAndServe(":8080", loggedHandler); err != nil {
//	    log.Fatalf("Server failed: %v", err)
//	}
package http
