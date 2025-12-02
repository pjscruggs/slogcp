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

package slogcphttp

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	stdhttp "net/http"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/pjscruggs/slogcp"
)

const instrumentationName = "github.com/pjscruggs/slogcp/slogcphttp"

var xCloudTraceContextExtractor = contextWithXCloudTrace

// Middleware returns an http.Handler middleware that derives a request-scoped
// logger, extracts trace context, and leaves application logging to handlers.
func Middleware(opts ...Option) func(stdhttp.Handler) stdhttp.Handler {
	cfg := applyOptions(opts)

	projectID := resolveProjectID(cfg.projectID)

	return func(next stdhttp.Handler) stdhttp.Handler {
		if next == nil {
			next = stdhttp.NotFoundHandler()
		}

		loggingHandler := buildLoggingHandler(cfg, projectID, next)
		handlerChain := wrapWithOTel(cfg, loggingHandler)

		return stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
			ctx := r.Context()
			if newCtx, _ := ensureSpanContext(ctx, r, cfg); newCtx != ctx {
				r = r.WithContext(newCtx)
			}
			handlerChain.ServeHTTP(w, r)
		})
	}
}

// resolveProjectID derives a project ID from configuration or runtime detection.
func resolveProjectID(configured string) string {
	projectID := strings.TrimSpace(configured)
	if projectID != "" {
		return projectID
	}
	return strings.TrimSpace(slogcp.DetectRuntimeInfo().ProjectID)
}

// buildLoggingHandler constructs the logging middleware around the next handler.
func buildLoggingHandler(cfg *config, projectID string, next stdhttp.Handler) stdhttp.Handler {
	return stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		start := time.Now()
		ctx := r.Context()
		scope := newRequestScope(r, start, cfg)

		attrs := buildRequestAttributes(cfg, projectID, r, scope)
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
}

// buildRequestAttributes assembles request-scoped attributes including enrichers.
func buildRequestAttributes(cfg *config, projectID string, r *stdhttp.Request, scope *RequestScope) []slog.Attr {
	traceAttrs, _ := slogcp.TraceAttributes(r.Context(), projectID)
	attrs := scope.loggerAttrs(cfg, traceAttrs)
	attrs = applyRequestEnrichers(cfg, attrs, r, scope)
	if cfg.includeHTTPRequestAttr {
		if attr := HTTPRequestAttr(r, scope); attr.Key != "" {
			attrs = append(attrs, attr)
		}
	}
	return applyRequestTransformers(cfg, attrs, r, scope)
}

// applyRequestEnrichers appends attributes produced by attrEnrichers.
func applyRequestEnrichers(cfg *config, attrs []slog.Attr, r *stdhttp.Request, scope *RequestScope) []slog.Attr {
	for _, enricher := range cfg.attrEnrichers {
		if enricher == nil {
			continue
		}
		if extra := enricher(r, scope); len(extra) > 0 {
			attrs = append(attrs, extra...)
		}
	}
	return attrs
}

// applyRequestTransformers feeds attributes through configured transformers.
func applyRequestTransformers(cfg *config, attrs []slog.Attr, r *stdhttp.Request, scope *RequestScope) []slog.Attr {
	for _, transformer := range cfg.attrTransformers {
		if transformer == nil {
			continue
		}
		attrs = transformer(attrs, r, scope)
	}
	return attrs
}

// wrapWithOTel wraps handler with otelhttp middleware when enabled.
func wrapWithOTel(cfg *config, handler stdhttp.Handler) stdhttp.Handler {
	if !cfg.enableOTel {
		return handler
	}

	return otelhttp.NewHandler(handler, instrumentationName, otelOptions(cfg)...)
}

// otelOptions builds OpenTelemetry handler options from configuration.
func otelOptions(cfg *config) []otelhttp.Option {
	var otelOpts []otelhttp.Option
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
	return otelOpts
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
		scope.populateFromRequest(r, cfg)
	}

	scope.status.Store(stdhttp.StatusOK)
	scope.latencyNS.Store(-1)
	return scope
}

// populateFromRequest copies request metadata into the scope.
func (rs *RequestScope) populateFromRequest(r *stdhttp.Request, cfg *config) {
	rs.requestSize = r.ContentLength
	rs.method = r.Method
	rs.populateURLFields(r)
	rs.userAgent = r.UserAgent()
	if cfg.includeClientIP {
		rs.clientIP = extractIP(r.RemoteAddr)
	}
	if cfg.routeGetter != nil {
		rs.route = strings.TrimSpace(cfg.routeGetter(r))
	}
}

// populateURLFields fills URL-derived fields including scheme and host.
func (rs *RequestScope) populateURLFields(r *stdhttp.Request) {
	if r.URL == nil {
		return
	}
	rs.target = r.URL.Path
	rs.query = r.URL.RawQuery
	rs.scheme = r.URL.Scheme
	if rs.scheme == "" {
		rs.scheme = inferScheme(r)
	}
	rs.host = r.Host
}

// inferScheme determines http/https scheme based on TLS presence.
func inferScheme(r *stdhttp.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// loggerAttrs assembles the structured log attributes for the request scope.
func (rs *RequestScope) loggerAttrs(cfg *config, traceAttrs []slog.Attr) []slog.Attr {
	attrs := make([]slog.Attr, 0, len(traceAttrs)+10)
	attrs = append(attrs, traceAttrs...)
	attrs = append(attrs, rs.requestCoreAttrs(cfg)...)
	attrs = append(attrs, rs.metricAttrs()...)
	attrs = append(attrs, rs.sizeAttrs()...)
	attrs = append(attrs, rs.clientAttrs(cfg)...)
	return attrs
}

// requestCoreAttrs returns method/target/route/scheme/host attributes.
func (rs *RequestScope) requestCoreAttrs(cfg *config) []slog.Attr {
	var attrs []slog.Attr
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
	return attrs
}

// metricAttrs supplies status, latency, and response size attributes.
func (rs *RequestScope) metricAttrs() []slog.Attr {
	return []slog.Attr{
		{
			Key: "http.status_code",
			Value: slog.AnyValue(logValueFunc(func() slog.Value {
				return slog.IntValue(rs.Status())
			})),
		},
		{
			Key: "http.latency",
			Value: slog.AnyValue(logValueFunc(func() slog.Value {
				return slog.DurationValue(rs.Latency())
			})),
		},
		{
			Key: "http.response_size",
			Value: slog.AnyValue(logValueFunc(func() slog.Value {
				return slog.Int64Value(rs.ResponseSize())
			})),
		},
	}
}

// sizeAttrs reports request size when available.
func (rs *RequestScope) sizeAttrs() []slog.Attr {
	if rs.requestSize <= 0 {
		return nil
	}
	return []slog.Attr{slog.Int64("http.request_size", rs.requestSize)}
}

// clientAttrs emits optional network and user agent attributes.
func (rs *RequestScope) clientAttrs(cfg *config) []slog.Attr {
	var attrs []slog.Attr
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
	if err != nil {
		return n, fmt.Errorf("write response body: %w", err)
	}
	return n, nil
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
		if err != nil {
			return n, fmt.Errorf("read from body: %w", err)
		}
		return n, nil
	}
	if !rr.wroteHeader {
		rr.WriteHeader(stdhttp.StatusOK)
	}
	n, err := io.Copy(rr.ResponseWriter, src)
	if n > 0 {
		rr.bytesWritten += n
		rr.scope.addResponseBytes(n)
	}
	if err != nil {
		return n, fmt.Errorf("copy response body: %w", err)
	}
	return n, nil
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
		conn, rw, err := hijacker.Hijack()
		if err != nil {
			return nil, nil, fmt.Errorf("hijack connection: %w", err)
		}
		return conn, rw, nil
	}
	return nil, nil, stdhttp.ErrNotSupported
}

// Push forwards HTTP/2 push requests when the underlying writer supports http.Pusher.
func (rr *responseRecorder) Push(target string, opts *stdhttp.PushOptions) error {
	if pusher, ok := rr.ResponseWriter.(stdhttp.Pusher); ok {
		if err := pusher.Push(target, opts); err != nil {
			return fmt.Errorf("http/2 push: %w", err)
		}
		return nil
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
	if cfg != nil && !cfg.propagateTrace {
		return ctx, trace.SpanContextFromContext(ctx)
	}

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
		if xctx, ok := xCloudTraceContextExtractor(ctx, header); ok {
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
