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

package http

import (
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"cloud.google.com/go/logging"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// responseWriter wraps an http.ResponseWriter to capture the HTTP status code
// written by the handler and the total size of the response body.
// It ensures that a status code (defaulting to 200 OK) is recorded even if
// WriteHeader is not explicitly called by the handler.
type responseWriter struct {
	http.ResponseWriter
	statusCode  int   // Stores the status code written.
	size        int64 // Stores the total bytes written to the response body.
	wroteHeader bool  // Tracks whether WriteHeader was called.
}

// WriteHeader records the statusCode and calls the underlying ResponseWriter's
// WriteHeader method. It prevents multiple calls from affecting the recorded
// status code or writing the header multiple times.
func (rw *responseWriter) WriteHeader(statusCode int) {
	if rw.wroteHeader {
		return
	}
	rw.statusCode = statusCode
	rw.ResponseWriter.WriteHeader(statusCode)
	rw.wroteHeader = true
}

// Write calls the underlying ResponseWriter's Write method, adding the number
// of bytes written to the tracked size. It ensures WriteHeader(200) is called
// first if no header has been written yet, matching the behavior of the
// standard library's http.Server.
func (rw *responseWriter) Write(b []byte) (int, error) {
	if !rw.wroteHeader {
		// Default to 200 OK if Write is called before WriteHeader.
		rw.WriteHeader(http.StatusOK)
	}
	n, err := rw.ResponseWriter.Write(b)
	rw.size += int64(n)
	return n, err
}

// Middleware returns a standard [http.Handler] middleware function.
// This middleware wraps an existing handler to log information about each
// incoming HTTP request and its response using the provided [slog.Logger].
// It is designed to work with the slogcp handler, which recognizes the
// special httpRequestKey attribute.
//
// For each request, it performs the following actions:
//  1. Extracts trace context (Trace ID, Span ID, sampling decision) from
//     incoming headers using the globally configured OpenTelemetry propagator
//     (e.g., W3C TraceContext, B3) and adds it to the request's context.
//     If no valid span context is present after extraction, it falls back to
//     parsing the legacy X-Cloud-Trace-Context header.
//  2. Records the start time of the request.
//  3. Wraps the [http.ResponseWriter] using the internal responseWriter type
//     to capture the status code and response size.
//  4. Calls the next handler in the chain with the wrapped writer and updated context.
//  5. Calculates the request duration after the handler returns.
//  6. Determines the final log level based on the HTTP status code (5xx=Error, 4xx=Warn, others=Info).
//  7. Constructs a [logging.HTTPRequest] struct containing request details (method, URL,
//     size, status, latency, remote IP, user agent, referer, etc.). It leverages
//     the underlying library's ability to populate fields from the original *http.Request.
//  8. Logs a final message ("HTTP request processed") using the provided logger's
//     LogAttrs method. The context containing the trace information is passed.
//     The [logging.HTTPRequest] struct is included as a special attribute
//     using the key defined by [httpRequestKey]. The core slogcp handler recognizes
//     this key and populates the corresponding field in the Cloud Logging entry,
//     removing the attribute from the JSON payload. The request duration is also
//     logged as a separate attribute for convenience.
//
// The logger passed to this function should typically be derived from the main
// slogcp package (e.g., `slogcpLogger.Logger`) to ensure logs are correctly
// formatted for GCP and the special httpRequestKey is handled.
func Middleware(logger *slog.Logger) func(http.Handler) http.Handler {
	// Get the globally configured OpenTelemetry propagator once during middleware creation.
	propagator := otel.GetTextMapPropagator()

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			startTime := time.Now()

			// Extract trace context from incoming headers via the global propagator.
			ctx := propagator.Extract(r.Context(), propagation.HeaderCarrier(r.Header))

			// If no valid span context after extraction, try legacy X-Cloud-Trace-Context header.
			if sc := trace.SpanContextFromContext(ctx); !sc.IsValid() {
				if header := r.Header.Get(XCloudTraceContextHeader); header != "" {
					ctx = injectTraceContextFromHeader(ctx, header)
				}
			}

			// Replace the request's context with the new one containing trace info.
			r = r.WithContext(ctx)

			// Wrap the original ResponseWriter to capture status code and response size.
			rw := &responseWriter{
				ResponseWriter: w,
				statusCode:     http.StatusOK, // Default to 200 OK.
				wroteHeader:    false,
			}

			// Delegate request handling to the next handler.
			next.ServeHTTP(rw, r)

			// Calculate duration after the handler finishes.
			duration := time.Since(startTime)

			// Determine the final status code captured by the responseWriter.
			finalStatusCode := rw.statusCode

			// Assemble the structured HTTP request details for Cloud Logging.
			// Setting the Request field allows the logging library to automatically
			// extract Method, RequestURL, UserAgent, Referer, Protocol.
			reqStruct := &logging.HTTPRequest{
				Request:      r,                       // Pass the original request for auto-population.
				RequestSize:  r.ContentLength,         // Explicitly set request size.
				Status:       finalStatusCode,         // Set the captured status code.
				ResponseSize: rw.size,                 // Set the captured response size.
				Latency:      duration,                // Set the calculated latency.
				RemoteIP:     extractIP(r.RemoteAddr), // Set the parsed remote IP.
				// ServerIP, CacheHit, CacheValidatedWithOriginServer are not populated here.
			}

			// Determine the log level based on the response status code.
			level := slog.LevelInfo
			if finalStatusCode >= 500 {
				level = slog.LevelError
			} else if finalStatusCode >= 400 {
				level = slog.LevelWarn
			}

			// Log the request completion details.
			// The context (ctx) carries trace information.
			// The reqStruct is passed via slog.Any with the special httpRequestKey.
			// The core slogcp handler uses these to populate the Cloud Logging entry.
			logger.LogAttrs(ctx, level, "HTTP request processed",
				slog.Any(httpRequestKey, reqStruct), // Special attribute for the handler.
				slog.Duration("duration", duration), // Log duration separately for convenience.
			)
		})
	}
}

// extractIP attempts to parse an IP address from a string typically in the
// format "IP:port" or "[IPv6]:port" as found in [http.Request.RemoteAddr].
// It returns only the IP address part as a string. If parsing fails (e.g., for
// Unix domain sockets or malformed addresses), it returns the original input string.
func extractIP(addr string) string {
	if addr == "" {
		return ""
	}

	// Handle IPv6 addresses in brackets, e.g., "[::1]:8080".
	if strings.HasPrefix(addr, "[") {
		endBracket := strings.Index(addr, "]")
		if endBracket > 0 {
			ipStr := addr[1:endBracket]
			if ip := net.ParseIP(ipStr); ip != nil {
				return ipStr // Return the IPv6 part.
			}
		}
		// Fall through if brackets are present but content isn't a valid IP.
	}

	// Try standard "host:port" format using net.SplitHostPort.
	host, _, err := net.SplitHostPort(addr)
	if err == nil {
		// Check if the extracted host is itself a valid IP.
		if ip := net.ParseIP(host); ip != nil {
			return host // Return the host part if it's a valid IP.
		}
		// If SplitHostPort succeeded but host isn't a valid IP (e.g., a domain name),
		// return the original address, as it's not just an IP:port.
		return addr
	}

	// If SplitHostPort failed, check if the address string itself is a valid IP.
	// This handles cases where the port is missing (e.g., "192.0.2.1").
	if ip := net.ParseIP(addr); ip != nil {
		return ip.String() // Return the valid IP string.
	}

	// If no parsing method worked (e.g., Unix socket path),
	// return the original address string as a fallback.
	return addr
}

// httpRequestKey is the attribute key used when logging the [logging.HTTPRequest]
// struct via [slog.Any]. The core slogcp handler specifically looks for this key
// to extract the struct and populate the corresponding `httpRequest` field in the
// Cloud Logging entry, removing this attribute from the final JSON payload.
const httpRequestKey = "httpRequest"
