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
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/logging"
	"github.com/pjscruggs/slogcp"
	"github.com/pjscruggs/slogcp/internal/gcp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

const instrumentationName = "github.com/pjscruggs/slogcp/http"

const (
	captureBufferMaxPrealloc = 64 << 10
	defaultAttrSliceCap      = 12
)

var cappedBufferPool = sync.Pool{
	New: func() any {
		return &cappedBuffer{}
	},
}

var attrSlicePool = sync.Pool{
	New: func() any {
		slice := make([]slog.Attr, 0, defaultAttrSliceCap)
		return &slice
	},
}

// responseWriter wraps an http.ResponseWriter to capture the HTTP status code
// written by the handler, the total size of the response body, and an optional
// excerpt of the body itself. It ensures that a status code (defaulting to 200
// OK) is recorded even if WriteHeader is not explicitly called by the handler.
type responseWriter struct {
	http.ResponseWriter
	statusCode  int           // Stores the status code written.
	size        int64         // Stores the total bytes written to the response body.
	wroteHeader bool          // Tracks whether WriteHeader was called.
	body        *cappedBuffer // Optional buffer for capturing response payloads.
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
// standard library's http.Server. When configured, it also captures up to the
// configured limit of response bytes for logging.
func (rw *responseWriter) Write(b []byte) (int, error) {
	if !rw.wroteHeader {
		// Default to 200 OK if Write is called before WriteHeader.
		rw.WriteHeader(http.StatusOK)
	}
	n, err := rw.ResponseWriter.Write(b)
	rw.size += int64(n)
	if rw.body != nil && n > 0 {
		_, _ = rw.body.Write(b[:n])
	}
	return n, err
}

// Middleware returns a standard [http.Handler] middleware function. This
// middleware wraps an existing handler to log information about each incoming
// HTTP request and its response using the provided [slog.Logger]. It is
// designed to work with the slogcp handler, which recognizes the special
// httpRequestKey attribute.
//
// Behaviour can be tuned with functional options or matching environment
// variables. By default the middleware logs every request without capturing
// bodies, does not recover from panics, and relies on the remote address for
// client IP detection. Server errors (5xx) are always logged even when
// trace-based suppression is enabled.
//
// For each request, the middleware:
//  1. Extracts trace context (Trace ID, Span ID, sampling decision) from
//     incoming headers using the globally configured OpenTelemetry propagator.
//     If no valid span context is present after extraction, it falls back to
//     parsing the legacy X-Cloud-Trace-Context header.
//  2. Applies configured filters to decide whether the request should be
//     logged. When logging is disabled but panic recovery is enabled, the
//     handler still wraps the request to surface panics.
//  3. Optionally tees the request body and wraps the [http.ResponseWriter] to
//     capture status code, response size, and response body excerpts.
//  4. Delegates request handling to the next handler in the chain.
//  5. Calculates request latency, determines the log level from the status
//     code (5xx=Error, 4xx=Warn, others=Info), and applies sampling or logger
//     level gating before emitting a log entry.
//  6. Logs a final message including the [logging.HTTPRequest] struct (via the
//     special httpRequestKey attribute) along with optional request/response
//     headers, body excerpts, and panic information when recovery is enabled.
//
// Option helpers such as [WithShouldLog], [WithSkipPathSubstrings],
// [WithSuppressUnsampledBelow], [WithLogRequestHeaderKeys],
// [WithLogResponseHeaderKeys], [WithRequestBodyLimit],
// [WithResponseBodyLimit], [WithRecoverPanics], and
// [WithTrustProxyHeaders] provide programmatic control over the same
// behaviours exposed through environment variables.
func Middleware(logger *slog.Logger, opts ...Option) func(http.Handler) http.Handler {
	envOptions := loadMiddlewareOptionsFromEnv()
	merged := defaultMiddlewareOptions()
	merged.SkipPathSubstrings = append([]string(nil), envOptions.SkipPathSubstrings...)
	merged.SuppressUnsampledBelow = envOptions.SuppressUnsampledBelow
	merged.LogRequestHeaderKeys = append([]string(nil), envOptions.LogRequestHeaderKeys...)
	merged.LogResponseHeaderKeys = append([]string(nil), envOptions.LogResponseHeaderKeys...)
	merged.RequestBodyLimit = envOptions.RequestBodyLimit
	merged.ResponseBodyLimit = envOptions.ResponseBodyLimit
	merged.RecoverPanics = envOptions.RecoverPanics
	merged.TrustProxyHeaders = envOptions.TrustProxyHeaders

	for _, opt := range opts {
		if opt != nil {
			opt(&merged)
		}
	}

	propagator := otel.GetTextMapPropagator()
	tracer := merged.Tracer
	if tracer == nil {
		tracer = otel.Tracer(instrumentationName)
	}
	traceProjectID := merged.TraceProjectID

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := propagator.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
			if sc := trace.SpanContextFromContext(ctx); !sc.IsValid() {
				if header := r.Header.Get(XCloudTraceContextHeader); header != "" {
					ctx = injectTraceContextFromHeader(ctx, header)
				}
			}
			if merged.StartSpanIfAbsent && !trace.SpanContextFromContext(ctx).IsValid() {
				spanName := fmt.Sprintf("%s %s", r.Method, r.URL.Path)
				var span trace.Span
				ctx, span = tracer.Start(ctx, spanName, trace.WithSpanKind(trace.SpanKindServer))
				defer span.End()
			}
			r = r.WithContext(ctx)

			shouldLog := merged.ShouldLog == nil || merged.ShouldLog(ctx, r)
			if shouldLog && len(merged.SkipPathSubstrings) > 0 {
				path := r.URL.Path
				for _, substr := range merged.SkipPathSubstrings {
					if substr != "" && strings.Contains(path, substr) {
						shouldLog = false
						break
					}
				}
			}

			if shouldLog && merged.SkipGoogleHealthChecks && isGoogleHealthCheckRequest(r) {
				shouldLog = false
			}

			if !shouldLog && !merged.RecoverPanics {
				next.ServeHTTP(w, r)
				return
			}

			startTime := time.Now()

			captureBodies := shouldLog || merged.RecoverPanics

			var requestBodyBuf *cappedBuffer
			if captureBodies && merged.RequestBodyLimit > 0 && r.Body != nil {
				requestBodyBuf = newCappedBuffer(merged.RequestBodyLimit)
				if requestBodyBuf != nil {
					defer putCappedBuffer(requestBodyBuf)
					originalBody := r.Body
					r.Body = &teeReadCloser{
						Reader: io.TeeReader(originalBody, requestBodyBuf),
						Closer: originalBody,
					}
				}
			}

			var responseBodyBuf *cappedBuffer
			if captureBodies {
				responseBodyBuf = newCappedBuffer(merged.ResponseBodyLimit)
				if responseBodyBuf != nil {
					defer putCappedBuffer(responseBodyBuf)
				}
			}

			rw := &responseWriter{
				ResponseWriter: w,
				statusCode:     http.StatusOK,
				body:           responseBodyBuf,
			}

			if merged.AttachLogger {
				traceAttrs, hasTrace := slogcp.TraceAttributes(ctx, traceProjectID)
				requestLogger := logger
				if hasTrace {
					requestLogger = withAttrs(requestLogger, traceAttrs)
				}
				requestLogger = requestLogger.With(
					slog.String("http.request.method", r.Method),
					slog.String("http.route", r.URL.Path),
				)
				ctx = slogcp.ContextWithLogger(ctx, requestLogger)
				r = r.WithContext(ctx)
			}

			var recovered any
			var stack string

			if merged.RecoverPanics {
				func() {
					defer func() {
						if rec := recover(); rec != nil {
							recovered = rec
							stack, _ = gcp.CaptureStack(nil)
							if !rw.wroteHeader {
								rw.WriteHeader(http.StatusInternalServerError)
							}
						}
					}()
					next.ServeHTTP(rw, r)
				}()
			} else {
				next.ServeHTTP(rw, r)
			}

			duration := time.Since(startTime)
			finalStatusCode := rw.statusCode

			level := slog.LevelInfo
			serverError := finalStatusCode >= 500
			if serverError {
				level = slog.LevelError
			} else if finalStatusCode >= 400 {
				level = slog.LevelWarn
			}

			if recovered != nil {
				level = slog.LevelError
			}

			spanCtx := trace.SpanContextFromContext(ctx)
			unsampled := spanCtx.IsValid() && !spanCtx.IsSampled()

			emitLog := shouldLog || recovered != nil
			if emitLog && recovered == nil {
				if threshold := merged.SuppressUnsampledBelow; threshold != nil && unsampled && !serverError && level < *threshold {
					emitLog = false
				}
			}
			if emitLog && !logger.Enabled(ctx, level) {
				emitLog = false
			}
			if !emitLog {
				return
			}

			trustProxy := merged.TrustProxyHeaders
			if !trustProxy && merged.TrustProxyDecisionFunc != nil && merged.TrustProxyDecisionFunc(r) {
				trustProxy = true
			}

			reqStruct := &logging.HTTPRequest{
				Request:      r,
				RequestSize:  r.ContentLength,
				Status:       finalStatusCode,
				ResponseSize: rw.size,
				Latency:      duration,
				RemoteIP:     resolveRemoteIP(r, trustProxy, merged.TrustProxyDecisionFunc),
			}

			attrsPtr := attrSlicePool.Get().(*[]slog.Attr)
			attrs := (*attrsPtr)[:0]

			attrs = append(attrs,
				slog.Any(httpRequestKey, reqStruct),
				slog.Duration("duration", duration),
			)

			if attr, ok := headersGroupAttr("requestHeaders", r.Header, merged.LogRequestHeaderKeys); ok {
				attrs = append(attrs, attr)
			}
			if attr, ok := headersGroupAttr("responseHeaders", rw.Header(), merged.LogResponseHeaderKeys); ok {
				attrs = append(attrs, attr)
			}

			if requestBodyBuf != nil && (requestBodyBuf.Len() > 0 || requestBodyBuf.Truncated()) {
				attrs = append(attrs, slog.String("requestBody", requestBodyBuf.String()))
				if requestBodyBuf.Truncated() {
					attrs = append(attrs, slog.Bool("requestBodyTruncated", true))
				}
			}
			if responseBodyBuf != nil && (responseBodyBuf.Len() > 0 || responseBodyBuf.Truncated()) {
				attrs = append(attrs, slog.String("responseBody", responseBodyBuf.String()))
				if responseBodyBuf.Truncated() {
					attrs = append(attrs, slog.Bool("responseBodyTruncated", true))
				}
			}

			message := "HTTP request processed"
			if recovered != nil {
				message = "HTTP panic recovered"
				attrs = append(attrs,
					slog.Any("panic", recovered),
					slog.String("panicStack", stack),
				)
			}

			logger.LogAttrs(ctx, level, message, attrs...)

			*attrsPtr = attrs[:0]
			attrSlicePool.Put(attrsPtr)
		})
	}
}

// resolveRemoteIP returns the best-effort client IP address for the provided
// request. When trustProxyHeaders is true, the function prefers values from
// X-Forwarded-For and X-Real-IP headers before falling back to RemoteAddr.
func resolveRemoteIP(r *http.Request, trustProxyHeaders bool, trustProxyFunc func(*http.Request) bool) string {
	if trustProxyFunc != nil && trustProxyFunc(r) {
		trustProxyHeaders = true
	}
	if trustProxyHeaders {
		if values := r.Header.Values("X-Forwarded-For"); len(values) > 0 {
			for _, value := range values {
				for _, part := range strings.Split(value, ",") {
					candidate := strings.TrimSpace(part)
					if candidate == "" {
						continue
					}
					if ip := net.ParseIP(candidate); ip != nil {
						return candidate
					}
				}
			}
		}
		if candidate := strings.TrimSpace(r.Header.Get("X-Real-IP")); candidate != "" {
			if ip := net.ParseIP(candidate); ip != nil {
				return candidate
			}
		}
	}
	return extractIP(r.RemoteAddr)
}

func withAttrs(logger *slog.Logger, attrs []slog.Attr) *slog.Logger {
	if logger == nil || len(attrs) == 0 {
		return logger
	}
	args := make([]any, len(attrs))
	for i, attr := range attrs {
		args[i] = attr
	}
	return logger.With(args...)
}

func isGoogleHealthCheckRequest(r *http.Request) bool {
	ua := r.Header.Get("User-Agent")
	if strings.HasPrefix(ua, "GoogleHC/") || strings.HasPrefix(ua, "GoogleHC-") {
		return true
	}
	if ua == "" && strings.EqualFold(r.Method, http.MethodGet) && r.URL != nil && r.URL.Path == "/" {
		if strings.EqualFold(r.Header.Get("Via"), "1.1 google") {
			return true
		}
	}
	return false
}

func isGoogleProxyRequest(r *http.Request) bool {
	via := strings.ToLower(strings.TrimSpace(r.Header.Get("Via")))
	if via == "1.1 google" || strings.Contains(via, "google") {
		return true
	}
	forwardedBy := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Forwarded-By")))
	if forwardedBy != "" && strings.Contains(forwardedBy, "google") {
		return true
	}
	return false
}

// headersGroupAttr materialises a slog.Group containing the requested header
// keys. It returns false when no requested headers are present.
func headersGroupAttr(name string, header http.Header, keys []string) (slog.Attr, bool) {
	if len(keys) == 0 {
		return slog.Attr{}, false
	}
	attrs := make([]slog.Attr, 0, len(keys))
	for _, key := range keys {
		if key == "" {
			continue
		}
		if values, ok := header[key]; ok && len(values) > 0 {
			copied := append([]string(nil), values...)
			attrs = append(attrs, slog.Any(key, copied))
		}
	}
	if len(attrs) == 0 {
		return slog.Attr{}, false
	}
	return slog.Attr{
		Key:   name,
		Value: slog.GroupValue(attrs...),
	}, true
}

// teeReadCloser combines a reader and closer, allowing the middleware to wrap
// the request body with an io.TeeReader while preserving the original Close behaviour.
type teeReadCloser struct {
	io.Reader
	io.Closer
}

// cappedBuffer captures up to a configured limit of bytes and notes whether
// truncation occurred. Buffers are pooled to amortize allocations across
// requests.
type cappedBuffer struct {
	data      []byte
	remaining int64
	truncated bool
}

// newCappedBuffer acquires a cappedBuffer when limit is positive. A nil result
// signals that capture is disabled, which keeps call sites straightforward.
func newCappedBuffer(limit int64) *cappedBuffer {
	if limit <= 0 {
		return nil
	}
	cb := cappedBufferPool.Get().(*cappedBuffer)
	cb.reset(limit)
	return cb
}

func putCappedBuffer(cb *cappedBuffer) {
	if cb == nil {
		return
	}
	if cap(cb.data) > captureBufferMaxPrealloc {
		cb.data = nil
	} else {
		cb.data = cb.data[:0]
	}
	cb.remaining = 0
	cb.truncated = false
	cappedBufferPool.Put(cb)
}

func (cb *cappedBuffer) reset(limit int64) {
	cb.remaining = limit
	cb.truncated = false

	desired := limit
	if desired < 0 {
		desired = 0
	}
	if desired > int64(captureBufferMaxPrealloc) {
		desired = int64(captureBufferMaxPrealloc)
	}
	if desired == 0 {
		cb.data = cb.data[:0]
		return
	}
	if int64(cap(cb.data)) < desired {
		cb.data = make([]byte, 0, int(desired))
	} else {
		cb.data = cb.data[:0]
	}
}

// Write records payload bytes up to the configured limit and notes when
// truncation occurs. It always reports the original length so wrapped writers
// see consistent accounting.
func (cb *cappedBuffer) Write(p []byte) (int, error) {
	if cb == nil {
		return len(p), nil
	}
	if cb.remaining > 0 {
		writeLen := len(p)
		if int64(writeLen) > cb.remaining {
			writeLen = int(cb.remaining)
		}
		if writeLen > 0 {
			cb.data = append(cb.data, p[:writeLen]...)
			cb.remaining -= int64(writeLen)
		}
		if writeLen < len(p) {
			cb.truncated = true
		}
	} else if len(p) > 0 {
		cb.truncated = true
	}
	return len(p), nil
}

// String returns the buffered contents. A nil buffer yields an empty string.
func (cb *cappedBuffer) String() string {
	if cb == nil {
		return ""
	}
	return string(cb.data)
}

// Len reports the number of bytes captured so far.
func (cb *cappedBuffer) Len() int {
	if cb == nil {
		return 0
	}
	return len(cb.data)
}

// Truncated reports whether any data exceeded the configured limit.
func (cb *cappedBuffer) Truncated() bool {
	if cb == nil {
		return false
	}
	return cb.truncated
}

// extractIP attempts to parse an IP address from a string typically in the
// format "IP:port" or "[IPv6]:port" as found in [http.Request.RemoteAddr].
// It returns only the IP address part as a string. If parsing fails (e.g., for
// Unix domain sockets or malformed addresses), it returns the original input string.
func extractIP(addr string) string {
	if addr == "" {
		return ""
	}

	if strings.HasPrefix(addr, "[") {
		endBracket := strings.Index(addr, "]")
		if endBracket > 0 {
			ipStr := addr[1:endBracket]
			if ip := net.ParseIP(ipStr); ip != nil {
				return ipStr
			}
		}
	}

	host, _, err := net.SplitHostPort(addr)
	if err == nil {
		if ip := net.ParseIP(host); ip != nil {
			return host
		}
		return addr
	}

	if ip := net.ParseIP(addr); ip != nil {
		return ip.String()
	}

	return addr
}

// httpRequestKey is the attribute key used when logging the [logging.HTTPRequest]
// struct via [slog.Any]. The core slogcp handler specifically looks for this key
// to extract the struct and populate the corresponding `httpRequest` field in the
// Cloud Logging entry, removing this attribute from the final JSON payload.
const httpRequestKey = "httpRequest"
