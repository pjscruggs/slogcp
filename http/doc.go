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
// The package keeps request-scoped logging, trace correlation, and
// OpenTelemetry instrumentation aligned with Google Cloud Logging expectations.
// It exposes helpers for both servers and clients:
//   - [Middleware] derives a child slog.Logger per request, stores it in the
//     context via [slogcp.ContextWithLogger], and records a [RequestScope] with
//     method, route, latency, status, and payload metadata. When [WithOTel]
//     is enabled (the default) it wraps handlers with [otelhttp.NewHandler]
//     so spans and trace propagation work automatically. The middleware never
//     emits request logs by itself; it enriches application logs produced by
//     your handlers.
//   - [Transport] wraps an http.RoundTripper to inject W3C trace headers,
//     optionally synthesise legacy X-Cloud-Trace-Context headers, and expose a
//     request-scoped logger on outgoing contexts. Options such as
//     [WithAttrEnricher], [WithAttrTransformer], and [WithIncludeQuery] apply to
//     both middleware and transport instrumentation.
//   - [ScopeFromContext] retrieves the RequestScope captured for inbound or
//     outbound requests so handlers can inspect latency, status codes, and
//     payload sizes. [HTTPRequestAttr] converts that scope into the
//     `httpRequest` structured payload used by Cloud Logging when you need to
//     emit it manually.
//   - [InjectTraceContextMiddleware] remains available for deployments that
//     disable OpenTelemetry instrumentation yet still need to recognise the
//     legacy X-Cloud-Trace-Context header on ingress.
//
// Typical usage:
//
//	slogHandler, _ := slogcp.NewHandler(os.Stdout)
//	logger := slog.New(slogHandler)
//
//	mux := http.NewServeMux()
//	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
//	    slogcp.Logger(r.Context()).Info("health probe")
//	    w.WriteHeader(http.StatusNoContent)
//	})
//
//	serverHandler := slogcphttp.Middleware(
//	    slogcphttp.WithLogger(logger),
//	    slogcphttp.WithIncludeQuery(false),
//	)(mux)
//
//	server := &http.Server{
//	    Addr:    ":8080",
//	    Handler: serverHandler,
//	}
//
//	httpClient := &http.Client{
//	    Transport: slogcphttp.Transport(nil),
//	}
//	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://example.com", nil)
//	httpClient.Do(req)
package http
