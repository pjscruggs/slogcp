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

// Package http provides net/http integration for slogcp.
//
// The package offers:
//
//  1. [Middleware]: wraps an [http.Handler] to log request/response details
//     with an application logger. Behaviour is configurable via functional
//     options or environment variables such as SLOGCP_HTTP_SKIP_PATH_SUBSTRINGS,
//     SLOGCP_HTTP_SUPPRESS_UNSAMPLED_BELOW, SLOGCP_HTTP_LOG_REQUEST_HEADER_KEYS,
//     SLOGCP_HTTP_LOG_RESPONSE_HEADER_KEYS, SLOGCP_HTTP_REQUEST_BODY_LIMIT,
//     SLOGCP_HTTP_RESPONSE_BODY_LIMIT, SLOGCP_HTTP_RECOVER_PANICS, and
//     SLOGCP_HTTP_TRUST_PROXY_HEADERS.
//
//  2. [InjectTraceContextMiddleware]: extracts the legacy X-Cloud-Trace-Context
//     header and injects a remote span context into the request’s context.
//     Use this only when you are not already relying on OpenTelemetry HTTP
//     server instrumentation to do the same.
//
//  3. [NewTraceRoundTripper]: an opt-in client transport that propagates the
//     current span context on outbound requests (injects W3C traceparent and,
//     by default, X-Cloud-Trace-Context). This is useful when you are not
//     using a full OpenTelemetry HTTP client wrapper.
//
// These helpers keep inbound and outbound traffic correlated with the JSON
// payload produced by slogcp’s handler, maintaining trace IDs and the
// `httpRequest` structure expected by Google Cloud log sinks.
//
// # Basic Usage (server)
//
//	handler, err := slogcp.NewHandler(os.Stdout)
//	if err != nil {
//	    log.Fatalf("failed to create slogcp handler: %v", err)
//	}
//	defer handler.Close()
//
//	logger := slog.New(handler)
//
//	// Import the middleware package
//	import slogcphttp "github.com/pjscruggs/slogcp/http"
//
//	myHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
//	    w.WriteHeader(http.StatusOK)
//	    w.Write([]byte("Hello, world!"))
//	})
//
//	// The trace injector should come before the main logging middleware.
//	wrapped := slogcphttp.Middleware(logger)(
//	    slogcphttp.InjectTraceContextMiddleware()(myHandler),
//	)
//
//	log.Println("Starting server on :8080")
//	if err := http.ListenAndServe(":8080", wrapped); err != nil {
//	    log.Fatalf("server failed: %v", err)
//	}
//
// # Basic Usage (client)
//
//	// Wrap an HTTP client to propagate trace headers on outbound requests.
//	client := &http.Client{
//	    Transport: slogcphttp.NewTraceRoundTripper(nil), // wraps http.DefaultTransport
//	}
//	// Use client.Do(req.WithContext(ctx)) where ctx carries a span (or an injected remote span).
package http
