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
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	stdhttp "net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pjscruggs/slogcp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

const instrumentationName = "github.com/pjscruggs/slogcp/http"

// Middleware returns an http.Handler middleware that derives a request-scoped
// logger, extracts trace context, and leaves application logging to handlers.
func Middleware(opts ...Option) func(stdhttp.Handler) stdhttp.Handler {
	cfg := applyOptions(opts)

	projectID := strings.TrimSpace(cfg.projectID)
	if projectID == "" {
		projectID = strings.TrimSpace(slogcp.DetectRuntimeInfo().ProjectID)
	}

	return func(next stdhttp.Handler) stdhttp.Handler {
		if next == nil {
			next = stdhttp.NotFoundHandler()
		}

		loggingHandler := stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
			start := time.Now()

			ctx := r.Context()

			scope := newRequestScope(r, start, cfg)

			traceAttrs, _ := slogcp.TraceAttributes(ctx, projectID)

			attrs := scope.loggerAttrs(cfg, traceAttrs)
			for _, enricher := range cfg.attrEnrichers {
				if enricher == nil {
					continue
				}
				if extra := enricher(r, scope); len(extra) > 0 {
					attrs = append(attrs, extra...)
				}
			}
			for _, transformer := range cfg.attrTransformers {
				if transformer == nil {
					continue
				}
				attrs = transformer(attrs, r, scope)
			}

			requestLogger := loggerWithAttrs(cfg.logger, attrs)

			ctx = slogcp.ContextWithLogger(ctx, requestLogger)
			ctx = context.WithValue(ctx, requestScopeKey{}, scope)
			r = r.WithContext(ctx)

			wrapped, recorder := wrapResponseWriter(w, scope)

			defer func() {
				scope.finalize(recorder.Status(), recorder.BytesWritten(), time.Since(start))
			}()

			next.ServeHTTP(wrapped, r)
		})

		handlerChain := stdhttp.Handler(loggingHandler)

		if cfg.enableOTel {
			otelOpts := []otelhttp.Option{}
			if cfg.tracerProvider != nil {
				otelOpts = append(otelOpts, otelhttp.WithTracerProvider(cfg.tracerProvider))
			}
			if cfg.propagatorsSet && cfg.propagators != nil {
				otelOpts = append(otelOpts, otelhttp.WithPropagators(cfg.propagators))
			}
			if cfg.publicEndpoint {
				otelOpts = append(otelOpts, otelhttp.WithPublicEndpoint())
			}
			if cfg.spanNameFormatter != nil {
				otelOpts = append(otelOpts, otelhttp.WithSpanNameFormatter(cfg.spanNameFormatter))
			}
			for _, filter := range cfg.filters {
				if filter != nil {
					otelOpts = append(otelOpts, otelhttp.WithFilter(filter))
				}
			}

			handlerChain = otelhttp.NewHandler(handlerChain, instrumentationName, otelOpts...)
		}

		return stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
			// Ensure remote trace context is present before otelhttp observes the request.
			ctx := r.Context()
			if newCtx, _ := ensureSpanContext(ctx, r, cfg); newCtx != ctx {
				r = r.WithContext(newCtx)
			}
			handlerChain.ServeHTTP(w, r)
		})
	}
}

// RequestScope captures request metadata surfaced to handlers via context.
type RequestScope struct {
	start       time.Time
	method      string
	route       string
	target      string
	query       string
	scheme      string
	host        string
	clientIP    string
	userAgent   string
	requestSize int64

	status    atomic.Int64
	respBytes atomic.Int64
	latencyNS atomic.Int64
}

// newRequestScope builds a RequestScope capturing request metadata and defaults.
func newRequestScope(r *stdhttp.Request, start time.Time, cfg *config) *RequestScope {
	scope := &RequestScope{
		start: start,
	}

	if r != nil {
		scope.requestSize = r.ContentLength
		scope.method = r.Method
		if r.URL != nil {
			scope.target = r.URL.Path
			scope.query = r.URL.RawQuery
			scope.scheme = r.URL.Scheme
			if scope.scheme == "" {
				if r.TLS != nil {
					scope.scheme = "https"
				} else {
					scope.scheme = "http"
				}
			}
			scope.host = r.Host
		}
		scope.userAgent = r.UserAgent()
		if cfg.includeClientIP {
			scope.clientIP = extractIP(r.RemoteAddr)
		}
		if cfg.routeGetter != nil {
			scope.route = strings.TrimSpace(cfg.routeGetter(r))
		}
	}

	scope.status.Store(stdhttp.StatusOK)
	scope.latencyNS.Store(-1)
	return scope
}

// loggerAttrs assembles the structured log attributes for the request scope.
func (rs *RequestScope) loggerAttrs(cfg *config, traceAttrs []slog.Attr) []slog.Attr {
	attrs := make([]slog.Attr, 0, len(traceAttrs)+10)
	if len(traceAttrs) > 0 {
		attrs = append(attrs, traceAttrs...)
	}
	if rs.method != "" {
		attrs = append(attrs, slog.String("http.method", rs.method))
	}
	if rs.target != "" {
		attrs = append(attrs, slog.String("http.target", rs.target))
	}
	if cfg.includeQuery && rs.query != "" {
		attrs = append(attrs, slog.String("http.query", rs.query))
	}
	if rs.route != "" {
		attrs = append(attrs, slog.String("http.route", rs.route))
	}
	if rs.scheme != "" {
		attrs = append(attrs, slog.String("http.scheme", rs.scheme))
	}
	if rs.host != "" {
		attrs = append(attrs, slog.String("http.host", rs.host))
	}
	attrs = append(attrs, slog.Attr{
		Key: "http.status_code",
		Value: slog.AnyValue(logValueFunc(func() slog.Value {
			return slog.IntValue(rs.Status())
		})),
	})
	attrs = append(attrs, slog.Attr{
		Key: "http.latency",
		Value: slog.AnyValue(logValueFunc(func() slog.Value {
			return slog.DurationValue(rs.Latency())
		})),
	})
	attrs = append(attrs, slog.Attr{
		Key: "http.response_size",
		Value: slog.AnyValue(logValueFunc(func() slog.Value {
			return slog.Int64Value(rs.ResponseSize())
		})),
	})
	if rs.requestSize > 0 {
		attrs = append(attrs, slog.Int64("http.request_size", rs.requestSize))
	}
	if cfg.includeClientIP && rs.clientIP != "" {
		attrs = append(attrs, slog.String("network.peer.ip", rs.clientIP))
	}
	if cfg.includeUserAgent && rs.userAgent != "" {
		attrs = append(attrs, slog.String("http.user_agent", rs.userAgent))
	}
	return attrs
}

// Method returns the HTTP method.
func (rs *RequestScope) Method() string { return rs.method }

// Target returns the request path component.
func (rs *RequestScope) Target() string { return rs.target }

// Query returns the raw query string without the '?' prefix.
func (rs *RequestScope) Query() string { return rs.query }

// Route returns the resolved route template, if provided.
func (rs *RequestScope) Route() string { return rs.route }

// Status returns the response status code with a default of 200.
func (rs *RequestScope) Status() int {
	code := rs.status.Load()
	if code == 0 {
		return stdhttp.StatusOK
	}
	return int(code)
}

// Latency reports the elapsed time since the request started.
func (rs *RequestScope) Latency() time.Duration {
	ns := rs.latencyNS.Load()
	if ns > 0 {
		return time.Duration(ns)
	}
	return time.Since(rs.start)
}

// ResponseSize returns the number of bytes written to the client.
func (rs *RequestScope) ResponseSize() int64 {
	return rs.respBytes.Load()
}

// RequestSize returns the content length reported by the client.
func (rs *RequestScope) RequestSize() int64 {
	return rs.requestSize
}

// ClientIP returns the parsed remote address.
func (rs *RequestScope) ClientIP() string {
	return rs.clientIP
}

// UserAgent returns the request's User-Agent header.
func (rs *RequestScope) UserAgent() string {
	return rs.userAgent
}

// Start returns the time the request began processing.
func (rs *RequestScope) Start() time.Time {
	return rs.start
}

// setStatus records the response status, defaulting to 200 when unset.
func (rs *RequestScope) setStatus(code int) {
	if code <= 0 {
		code = stdhttp.StatusOK
	}
	rs.status.Store(int64(code))
}

// addResponseBytes accumulates response bytes if the delta is positive.
func (rs *RequestScope) addResponseBytes(delta int64) {
	if delta <= 0 {
		return
	}
	rs.respBytes.Add(delta)
}

// finalize stores the terminal status, byte count, and latency for the request.
func (rs *RequestScope) finalize(status int, bytes int64, d time.Duration) {
	rs.setStatus(status)
	if bytes >= 0 {
		rs.respBytes.Store(bytes)
	}
	if d < 0 {
		d = 0
	}
	rs.latencyNS.Store(d.Nanoseconds())
}

type requestScopeKey struct{}

// ScopeFromContext retrieves the RequestScope placed in the request context by
// the middleware.
func ScopeFromContext(ctx context.Context) (*RequestScope, bool) {
	if ctx == nil {
		return nil, false
	}
	scope, ok := ctx.Value(requestScopeKey{}).(*RequestScope)
	return scope, ok && scope != nil
}

type logValueFunc func() slog.Value

// LogValue implements slog.LogValuer for deferred attribute evaluation.
func (f logValueFunc) LogValue() slog.Value {
	return f()
}

type responseRecorder struct {
	stdhttp.ResponseWriter
	scope        *RequestScope
	status       int
	wroteHeader  bool
	bytesWritten int64
}

// WriteHeader records the status code before delegating to the wrapped writer.
func (rr *responseRecorder) WriteHeader(status int) {
	if rr.wroteHeader {
		rr.ResponseWriter.WriteHeader(status)
		return
	}
	rr.status = status
	rr.scope.setStatus(status)
	rr.ResponseWriter.WriteHeader(status)
	rr.wroteHeader = true
}

// Write records bytes written and forwards the call to the underlying writer.
func (rr *responseRecorder) Write(p []byte) (int, error) {
	if !rr.wroteHeader {
		rr.WriteHeader(stdhttp.StatusOK)
	}
	n, err := rr.ResponseWriter.Write(p)
	if n > 0 {
		rr.bytesWritten += int64(n)
		rr.scope.addResponseBytes(int64(n))
	}
	return n, err
}

// ReadFrom streams data from src while tracking bytes for logging.
func (rr *responseRecorder) ReadFrom(src io.Reader) (int64, error) {
	if rf, ok := rr.ResponseWriter.(io.ReaderFrom); ok {
		if !rr.wroteHeader {
			rr.WriteHeader(stdhttp.StatusOK)
		}
		n, err := rf.ReadFrom(src)
		if n > 0 {
			rr.bytesWritten += n
			rr.scope.addResponseBytes(n)
		}
		return n, err
	}
	if !rr.wroteHeader {
		rr.WriteHeader(stdhttp.StatusOK)
	}
	n, err := io.Copy(rr.ResponseWriter, src)
	if n > 0 {
		rr.bytesWritten += n
		rr.scope.addResponseBytes(n)
	}
	return n, err
}

// Status returns the HTTP status code that was written to the client.
func (rr *responseRecorder) Status() int {
	if rr.status == 0 {
		return stdhttp.StatusOK
	}
	return rr.status
}

// BytesWritten reports the cumulative number of bytes sent to the client.
func (rr *responseRecorder) BytesWritten() int64 {
	return rr.bytesWritten
}

// wrapResponseWriter decorates the ResponseWriter to capture response metadata and optional interfaces.
func wrapResponseWriter(w stdhttp.ResponseWriter, scope *RequestScope) (stdhttp.ResponseWriter, *responseRecorder) {
	rec := &responseRecorder{
		ResponseWriter: w,
		scope:          scope,
		status:         stdhttp.StatusOK,
	}
	scope.setStatus(stdhttp.StatusOK)
	return rec, rec
}

// Flush forwards the flush request to the underlying ResponseWriter when supported.
func (rr *responseRecorder) Flush() {
	if flusher, ok := rr.ResponseWriter.(stdhttp.Flusher); ok {
		flusher.Flush()
	}
}

// Hijack delegates to the wrapped Hijacker when supported, otherwise returns http.ErrNotSupported.
func (rr *responseRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hijacker, ok := rr.ResponseWriter.(stdhttp.Hijacker); ok {
		return hijacker.Hijack()
	}
	return nil, nil, stdhttp.ErrNotSupported
}

// Push forwards HTTP/2 push requests when the underlying writer supports http.Pusher.
func (rr *responseRecorder) Push(target string, opts *stdhttp.PushOptions) error {
	if pusher, ok := rr.ResponseWriter.(stdhttp.Pusher); ok {
		return pusher.Push(target, opts)
	}
	return stdhttp.ErrNotSupported
}

// CloseNotify exposes the wrapped CloseNotifier channel when available.
func (rr *responseRecorder) CloseNotify() <-chan bool {
	if cn, ok := rr.ResponseWriter.(interface{ CloseNotify() <-chan bool }); ok {
		return cn.CloseNotify()
	}
	return nil
}

// ensureSpanContext extracts existing span context or synthesizes one from incoming headers.
func ensureSpanContext(ctx context.Context, r *stdhttp.Request, cfg *config) (context.Context, trace.SpanContext) {
	sc := trace.SpanContextFromContext(ctx)
	if sc.IsValid() {
		return ctx, sc
	}

	propagator := cfg.propagators
	if propagator == nil {
		propagator = otel.GetTextMapPropagator()
	}
	if propagator != nil {
		extracted := propagator.Extract(ctx, propagation.HeaderCarrier(r.Header))
		sc = trace.SpanContextFromContext(extracted)
		if sc.IsValid() {
			return extracted, sc
		}
	}

	if header := r.Header.Get(XCloudTraceContextHeader); header != "" {
		if xctx, ok := contextWithXCloudTrace(ctx, header); ok {
			return xctx, trace.SpanContextFromContext(xctx)
		}
	}

	return ctx, sc
}

// extractIP strips the port from a host:port string and returns the host component.
func extractIP(addr string) string {
	if addr == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

// loggerWithAttrs returns a logger enriched with the supplied attributes.
func loggerWithAttrs(base *slog.Logger, attrs []slog.Attr) *slog.Logger {
	if base == nil {
		base = slog.Default()
	}
	if len(attrs) == 0 {
		return base
	}
	args := make([]any, len(attrs))
	for i, attr := range attrs {
		args[i] = attr
	}
	return base.With(args...)
}
