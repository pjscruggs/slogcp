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
	"net/http"
	"net/netip"
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

const (
	schemeHTTP  = "http"
	schemeHTTPS = "https"
)

var xCloudTraceContextExtractor = contextWithXCloudTrace

// Middleware returns an http.Handler middleware that derives a request-scoped
// logger, extracts trace context, and leaves application logging to handlers.
func Middleware(opts ...Option) func(http.Handler) http.Handler {
	cfg := applyOptions(opts)
	needsRuntimeInfo := strings.TrimSpace(cfg.projectID) == "" || (!cfg.trustXForwardedProtoSet && cfg.proxyMode != ProxyModeGCLB)
	var runtimeInfo slogcp.RuntimeInfo
	if needsRuntimeInfo {
		runtimeInfo = slogcp.DetectRuntimeInfo()
	}
	applyDefaultForwardedProtoTrust(cfg, runtimeInfo)

	projectID := resolveProjectID(cfg.projectID, runtimeInfo)

	return func(next http.Handler) http.Handler {
		if next == nil {
			next = http.NotFoundHandler()
		}

		loggingHandler := buildLoggingHandler(cfg, projectID, next)
		handlerChain := wrapWithOTel(cfg, loggingHandler)

		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			if newCtx, _ := ensureSpanContext(ctx, r, cfg); newCtx != ctx {
				r = r.WithContext(newCtx)
			}
			handlerChain.ServeHTTP(w, r)
		})
	}
}

// resolveProjectID derives a project ID from configuration or runtime detection.
func resolveProjectID(configured string, runtimeInfo slogcp.RuntimeInfo) string {
	projectID := strings.TrimSpace(configured)
	if projectID != "" {
		return projectID
	}
	return strings.TrimSpace(runtimeInfo.ProjectID)
}

// buildLoggingHandler constructs the logging middleware around the next handler.
func buildLoggingHandler(cfg *config, projectID string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

// applyDefaultForwardedProtoTrust enables X-Forwarded-Proto parsing when callers
// have not explicitly configured scheme inference and the process is running in
// a managed GCP serverless environment where TLS terminates before the request
// reaches the application (Cloud Run services, Cloud Functions, App Engine).
func applyDefaultForwardedProtoTrust(cfg *config, runtimeInfo slogcp.RuntimeInfo) {
	if cfg == nil {
		return
	}
	if cfg.trustXForwardedProtoSet {
		return
	}
	if cfg.proxyMode == ProxyModeGCLB {
		cfg.trustXForwardedProto = true
		return
	}
	cfg.trustXForwardedProto = shouldTrustXForwardedProtoByEnvironment(runtimeInfo.Environment)
}

// shouldTrustXForwardedProtoByEnvironment reports whether slogcphttp should treat
// X-Forwarded-Proto as a reliable scheme signal for the detected environment.
func shouldTrustXForwardedProtoByEnvironment(env slogcp.RuntimeEnvironment) bool {
	switch env {
	case slogcp.RuntimeEnvCloudRunService,
		slogcp.RuntimeEnvCloudFunctions,
		slogcp.RuntimeEnvAppEngineStandard,
		slogcp.RuntimeEnvAppEngineFlexible:
		return true
	default:
		return false
	}
}

// buildRequestAttributes assembles request-scoped attributes including enrichers.
func buildRequestAttributes(cfg *config, projectID string, r *http.Request, scope *RequestScope) []slog.Attr {
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
func applyRequestEnrichers(cfg *config, attrs []slog.Attr, r *http.Request, scope *RequestScope) []slog.Attr {
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
func applyRequestTransformers(cfg *config, attrs []slog.Attr, r *http.Request, scope *RequestScope) []slog.Attr {
	for _, transformer := range cfg.attrTransformers {
		if transformer == nil {
			continue
		}
		attrs = transformer(attrs, r, scope)
	}
	return attrs
}

// wrapWithOTel wraps handler with otelhttp middleware when enabled.
func wrapWithOTel(cfg *config, handler http.Handler) http.Handler {
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
	if cfg.propagateTrace {
		if cfg.propagatorsSet && cfg.propagators != nil {
			otelOpts = append(otelOpts, otelhttp.WithPropagators(cfg.propagators))
		}
	} else {
		otelOpts = append(otelOpts, otelhttp.WithPropagators(noopPropagator{}))
	}
	if cfg.publicEndpoint {
		otelOpts = append(otelOpts, otelhttp.WithPublicEndpointFn(func(*http.Request) bool {
			return true
		}))
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

type noopPropagator struct{}

// Inject satisfies propagation.TextMapPropagator while remaining a no-op.
func (noopPropagator) Inject(ctx context.Context, carrier propagation.TextMapCarrier) {
	_ = ctx
	_ = carrier
}

// Extract returns the provided context unchanged.
func (noopPropagator) Extract(ctx context.Context, _ propagation.TextMapCarrier) context.Context {
	return ctx
}

// Fields reports no injected fields.
func (noopPropagator) Fields() []string { return nil }

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
	peerPort    int
	outbound    bool
	userAgent   string
	requestSize int64

	status    atomic.Int64
	respBytes atomic.Int64
	latencyNS atomic.Int64
}

const unsetLatencySentinel = int64(-1)

// newRequestScope builds a RequestScope capturing request metadata and defaults.
func newRequestScope(r *http.Request, start time.Time, cfg *config) *RequestScope {
	scope := &RequestScope{
		start: start,
	}

	if r != nil {
		scope.populateFromRequest(r, cfg)
	}

	scope.status.Store(http.StatusOK)
	scope.latencyNS.Store(unsetLatencySentinel)
	return scope
}

// populateFromRequest copies request metadata into the scope.
func (rs *RequestScope) populateFromRequest(r *http.Request, cfg *config) {
	rs.requestSize = r.ContentLength
	rs.method = r.Method
	rs.populateURLFields(r, cfg)
	rs.userAgent = r.UserAgent()
	if cfg.includeClientIP {
		rs.clientIP = clientIPFromRequest(r, cfg)
	}
	if cfg.routeGetter != nil {
		rs.route = strings.TrimSpace(cfg.routeGetter(r))
	}
}

// populateURLFields fills URL-derived fields including scheme and host.
func (rs *RequestScope) populateURLFields(r *http.Request, cfg *config) {
	if r.URL == nil {
		return
	}
	rs.target = r.URL.Path
	rs.query = r.URL.RawQuery
	rs.scheme = r.URL.Scheme
	if rs.scheme == "" {
		rs.scheme = inferScheme(r, cfg)
	}
	rs.host = r.Host
}

// inferScheme determines the request scheme, preferring trusted proxy headers
// when configured, and otherwise falling back to TLS presence.
func inferScheme(r *http.Request, cfg *config) string {
	if r == nil {
		return ""
	}
	if cfg != nil && (cfg.proxyMode == ProxyModeGCLB || cfg.trustXForwardedProto) {
		if proto := xForwardedProto(r.Header.Get("X-Forwarded-Proto")); proto != "" {
			return proto
		}
	}
	if r.TLS != nil {
		return schemeHTTPS
	}
	return schemeHTTP
}

// loggerAttrs assembles the structured log attributes for the request scope.
func (rs *RequestScope) loggerAttrs(cfg *config, traceAttrs []slog.Attr) []slog.Attr {
	capHint := len(traceAttrs) + 14
	attrs := make([]slog.Attr, 0, capHint)
	if len(traceAttrs) > 0 {
		attrs = append(attrs, traceAttrs...)
	}
	attrs = rs.appendRequestCoreAttrs(cfg, attrs)
	attrs = rs.appendMetricAttrs(attrs)
	attrs = rs.appendSizeAttrs(attrs)
	attrs = rs.appendClientAttrs(cfg, attrs)
	return attrs
}

// appendRequestCoreAttrs appends method/target/route/scheme/host attributes.
func (rs *RequestScope) appendRequestCoreAttrs(cfg *config, attrs []slog.Attr) []slog.Attr {
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
	return rs.appendMetricAttrs(nil)
}

type requestScopeMetricKind uint8

const (
	requestScopeMetricStatus requestScopeMetricKind = iota
	requestScopeMetricResponseSize
)

type requestScopeMetricValue struct {
	scope *RequestScope
	kind  requestScopeMetricKind
}

// LogValue implements slog.LogValuer for deferred metric evaluation.
func (v requestScopeMetricValue) LogValue() slog.Value {
	if v.scope == nil {
		return slog.Value{}
	}
	switch v.kind {
	case requestScopeMetricStatus:
		return slog.IntValue(v.scope.Status())
	case requestScopeMetricResponseSize:
		return slog.Int64Value(v.scope.ResponseSize())
	default:
		return slog.Value{}
	}
}

// appendMetricAttrs appends status, latency, and response size attributes.
func (rs *RequestScope) appendMetricAttrs(attrs []slog.Attr) []slog.Attr {
	lat, finalized := rs.Latency()

	attrs = append(attrs,
		slog.Attr{
			Key:   "http.status_code",
			Value: slog.AnyValue(requestScopeMetricValue{scope: rs, kind: requestScopeMetricStatus}),
		},
		slog.Attr{
			Key:   "http.response_size",
			Value: slog.AnyValue(requestScopeMetricValue{scope: rs, kind: requestScopeMetricResponseSize}),
		},
	)

	if finalized {
		attrs = append(attrs, slog.Duration("http.latency", lat))
	}

	return attrs
}

// appendSizeAttrs appends the request size attribute when available.
func (rs *RequestScope) appendSizeAttrs(attrs []slog.Attr) []slog.Attr {
	if rs.requestSize <= 0 {
		return attrs
	}
	return append(attrs, slog.Int64("http.request_size", rs.requestSize))
}

// appendClientAttrs appends optional network and user agent attributes.
func (rs *RequestScope) appendClientAttrs(cfg *config, attrs []slog.Attr) []slog.Attr {
	if cfg.includeClientIP && rs.clientIP != "" {
		if rs.outbound {
			attrs = append(attrs, slog.String("server.address", rs.clientIP))
			if rs.peerPort > 0 {
				attrs = append(attrs, slog.Int("server.port", rs.peerPort))
			}
		} else {
			attrs = append(attrs, slog.String("network.peer.ip", rs.clientIP))
		}
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
		return http.StatusOK
	}
	return int(code)
}

// Latency returns the latency and whether it is finalized (true after finalize).
func (rs *RequestScope) Latency() (time.Duration, bool) {
	ns := rs.latencyNS.Load()
	if ns != unsetLatencySentinel {
		return time.Duration(ns), true
	}
	return time.Since(rs.start), false
}

// ResponseSize returns the number of bytes written to the client.
func (rs *RequestScope) ResponseSize() int64 {
	return rs.respBytes.Load()
}

// RequestSize returns the content length reported by the client.
func (rs *RequestScope) RequestSize() int64 {
	return rs.requestSize
}

// Scheme returns the resolved request scheme.
func (rs *RequestScope) Scheme() string { return rs.scheme }

// Host returns the request host.
func (rs *RequestScope) Host() string { return rs.host }

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
		code = http.StatusOK
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

type responseRecorder struct {
	http.ResponseWriter
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
		rr.WriteHeader(http.StatusOK)
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
			rr.WriteHeader(http.StatusOK)
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
		rr.WriteHeader(http.StatusOK)
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
		return http.StatusOK
	}
	return rr.status
}

// BytesWritten reports the cumulative number of bytes sent to the client.
func (rr *responseRecorder) BytesWritten() int64 {
	return rr.bytesWritten
}

// wrapResponseWriter decorates the ResponseWriter to capture response metadata and optional interfaces.
func wrapResponseWriter(w http.ResponseWriter, scope *RequestScope) (http.ResponseWriter, *responseRecorder) {
	rec := &responseRecorder{
		ResponseWriter: w,
		scope:          scope,
		status:         http.StatusOK,
	}
	scope.setStatus(http.StatusOK)
	return rec, rec
}

// Unwrap exposes the underlying ResponseWriter for http.ResponseController.
func (rr *responseRecorder) Unwrap() http.ResponseWriter {
	return rr.ResponseWriter
}

// Flush forwards the flush request to the underlying ResponseWriter when supported.
func (rr *responseRecorder) Flush() {
	if flusher, ok := rr.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Hijack delegates to the wrapped Hijacker when supported, otherwise returns http.ErrNotSupported.
func (rr *responseRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hijacker, ok := rr.ResponseWriter.(http.Hijacker); ok {
		conn, rw, err := hijacker.Hijack()
		if err != nil {
			return nil, nil, fmt.Errorf("hijack connection: %w", err)
		}
		return conn, rw, nil
	}
	return nil, nil, http.ErrNotSupported
}

// Push forwards HTTP/2 push requests when the underlying writer supports http.Pusher.
func (rr *responseRecorder) Push(target string, opts *http.PushOptions) error {
	if pusher, ok := rr.ResponseWriter.(http.Pusher); ok {
		if err := pusher.Push(target, opts); err != nil {
			return fmt.Errorf("http/2 push: %w", err)
		}
		return nil
	}
	return http.ErrNotSupported
}

// CloseNotify exposes the wrapped CloseNotifier channel when available.
func (rr *responseRecorder) CloseNotify() <-chan bool {
	if cn, ok := rr.ResponseWriter.(interface{ CloseNotify() <-chan bool }); ok {
		return cn.CloseNotify()
	}
	return nil
}

// ensureSpanContext extracts existing span context or synthesizes one from incoming headers.
func ensureSpanContext(ctx context.Context, r *http.Request, cfg *config) (context.Context, trace.SpanContext) {
	if cfg != nil {
		if !cfg.propagateTrace {
			return ctx, trace.SpanContextFromContext(ctx)
		}
	}

	sc := trace.SpanContextFromContext(ctx)
	if sc.IsValid() {
		return ctx, sc
	}

	if !shouldExtractRemoteSpanContext(cfg) {
		return ctx, sc
	}

	if r == nil {
		return ctx, sc
	}

	if extractedCtx, extractedSC, ok := extractSpanContextFromHeaders(ctx, r, cfg); ok {
		return extractedCtx, extractedSC
	}

	if xctx, xsc, ok := extractSpanContextFromXCloudTraceContext(ctx, r); ok {
		return xctx, xsc
	}

	return ctx, sc
}

// shouldExtractRemoteSpanContext reports whether ensureSpanContext should trust and extract remote context.
func shouldExtractRemoteSpanContext(cfg *config) bool {
	if cfg == nil {
		return true
	}
	if !cfg.publicEndpoint {
		return true
	}
	if cfg.enableOTel {
		return true
	}
	return cfg.trustRemoteTraceForLogs
}

// extractSpanContextFromHeaders extracts a span context using the configured/global propagator.
func extractSpanContextFromHeaders(ctx context.Context, r *http.Request, cfg *config) (context.Context, trace.SpanContext, bool) {
	var propagator propagation.TextMapPropagator
	if cfg != nil {
		propagator = cfg.propagators
		if propagator == nil && !cfg.propagatorsSet {
			propagator = otel.GetTextMapPropagator()
		}
	}
	if propagator == nil && cfg == nil {
		propagator = otel.GetTextMapPropagator()
	}
	if propagator == nil {
		return ctx, trace.SpanContextFromContext(ctx), false
	}

	extracted := propagator.Extract(ctx, propagation.HeaderCarrier(r.Header))
	sc := trace.SpanContextFromContext(extracted)
	if !sc.IsValid() {
		return ctx, trace.SpanContextFromContext(ctx), false
	}
	return extracted, sc, true
}

// extractSpanContextFromXCloudTraceContext extracts span context from X-Cloud-Trace-Context when present.
func extractSpanContextFromXCloudTraceContext(ctx context.Context, r *http.Request) (context.Context, trace.SpanContext, bool) {
	header := r.Header.Get(XCloudTraceContextHeader)
	if header == "" {
		return ctx, trace.SpanContextFromContext(ctx), false
	}

	extracted, ok := xCloudTraceContextExtractor(ctx, header)
	if !ok {
		return ctx, trace.SpanContextFromContext(ctx), false
	}
	sc := trace.SpanContextFromContext(extracted)
	if !sc.IsValid() {
		return ctx, trace.SpanContextFromContext(ctx), false
	}
	return extracted, sc, true
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

// clientIPFromRequest determines the client IP address for the request based on proxy mode.
func clientIPFromRequest(r *http.Request, cfg *config) string {
	if r == nil {
		return ""
	}
	if cfg != nil && cfg.proxyMode == ProxyModeGCLB {
		if ip := gclbClientIPFromXForwardedFor(r.Header.Get("X-Forwarded-For"), cfg.xffClientIPFromRight); ip != "" {
			return ip
		}
	}
	return extractIP(r.RemoteAddr)
}

// xForwardedProto parses X-Forwarded-Proto, returning a normalized http/https
// token when present.
func xForwardedProto(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if token, _, ok := strings.Cut(value, ","); ok {
		value = token
	}
	value = strings.ToLower(strings.TrimSpace(value))
	if value == schemeHTTP || value == schemeHTTPS {
		return value
	}
	return ""
}

// gclbClientIPFromXForwardedFor extracts the trusted client IP from X-Forwarded-For by selecting
// the Nth IP from the right and validating it as an IP address.
func gclbClientIPFromXForwardedFor(value string, fromRight int) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if fromRight <= 0 {
		fromRight = 1
	}

	parts := strings.Split(value, ",")
	cleaned := parts[:0]
	for _, part := range parts {
		part = strings.TrimSpace(part)
		part = strings.Trim(part, "\"")
		if part == "" {
			continue
		}
		cleaned = append(cleaned, part)
	}
	if len(cleaned) < fromRight {
		return ""
	}

	candidate := strings.TrimSpace(cleaned[len(cleaned)-fromRight])
	candidate = strings.Trim(candidate, "\"")
	candidate = strings.Trim(candidate, "[]")
	if candidate == "" {
		return ""
	}

	addr, err := netip.ParseAddr(candidate)
	if err != nil {
		return ""
	}
	return addr.String()
}

// loggerWithAttrs returns a logger enriched with the supplied attributes.
func loggerWithAttrs(base *slog.Logger, attrs []slog.Attr) *slog.Logger {
	if base == nil {
		base = slog.Default()
	}
	if len(attrs) == 0 {
		return base
	}
	handler := base.Handler().WithAttrs(attrs)
	return slog.New(handler)
}
