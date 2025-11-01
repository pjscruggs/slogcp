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
	"context"
	"log/slog"
	"net/http"
	"net/textproto"
	"os"
	"strconv"
	"strings"

	"github.com/pjscruggs/slogcp/chatter"
	"go.opentelemetry.io/otel/trace"
)

// ShouldLogFunc decides whether a request should emit a structured log entry.
// Returning false skips the final log message but still allows the handler to run.
type ShouldLogFunc func(context.Context, *http.Request) bool

// MiddlewareOptions controls the behaviour of [Middleware].
type MiddlewareOptions struct {
	ShouldLog              ShouldLogFunc
	LogRequests            bool
	SkipPathSubstrings     []string
	SuppressUnsampledBelow *slog.Level
	LogRequestHeaderKeys   []string
	LogResponseHeaderKeys  []string
	RequestBodyLimit       int64
	ResponseBodyLimit      int64
	RecoverPanics          bool
	TrustProxyHeaders      bool

	AttachLogger           bool
	StartSpanIfAbsent      bool
	Tracer                 trace.Tracer
	ChatterConfig          chatter.Config
	TrustProxyDecisionFunc func(*http.Request) bool
	TraceProjectID         string
	RouteGetter            func(*http.Request) string
}

// Option mutates MiddlewareOptions.
type Option func(*MiddlewareOptions)

// defaultMiddlewareOptions returns the zero-value configuration used before
// environment variables and functional options are applied.
func defaultMiddlewareOptions() MiddlewareOptions {
	return MiddlewareOptions{
		AttachLogger:           true,
		StartSpanIfAbsent:      true,
		ChatterConfig:          chatter.DefaultConfig(),
		TrustProxyDecisionFunc: isGoogleProxyRequest,
	}
}

// loadMiddlewareOptionsFromEnv builds MiddlewareOptions from the current
// process environment. Invalid values are ignored so functional options can
// supply overrides without additional error handling.
func loadMiddlewareOptionsFromEnv() MiddlewareOptions {
	opts := defaultMiddlewareOptions()

	if raw, ok := os.LookupEnv("SLOGCP_HTTP_SKIP_PATH_SUBSTRINGS"); ok {
		opts.SkipPathSubstrings = splitAndClean(raw)
	}
	if raw, ok := os.LookupEnv("SLOGCP_HTTP_LOG_REQUESTS"); ok {
		if v, ok := parseBoolFlag(raw); ok {
			opts.LogRequests = v
		}
	}
	if raw, ok := os.LookupEnv("SLOGCP_HTTP_SUPPRESS_UNSAMPLED_BELOW"); ok {
		if lvl, err := parseLevel(raw); err == nil {
			opts.SuppressUnsampledBelow = &lvl
		}
	}
	if raw, ok := os.LookupEnv("SLOGCP_HTTP_LOG_REQUEST_HEADER_KEYS"); ok {
		opts.LogRequestHeaderKeys = cleanHeaderKeys(strings.Split(raw, ","))
	}
	if raw, ok := os.LookupEnv("SLOGCP_HTTP_LOG_RESPONSE_HEADER_KEYS"); ok {
		opts.LogResponseHeaderKeys = cleanHeaderKeys(strings.Split(raw, ","))
	}
	if raw, ok := os.LookupEnv("SLOGCP_HTTP_REQUEST_BODY_LIMIT"); ok {
		if v, err := parsePositiveInt64(raw); err == nil {
			opts.RequestBodyLimit = v
		}
	}
	if raw, ok := os.LookupEnv("SLOGCP_HTTP_RESPONSE_BODY_LIMIT"); ok {
		if v, err := parsePositiveInt64(raw); err == nil {
			opts.ResponseBodyLimit = v
		}
	}
	if raw, ok := os.LookupEnv("SLOGCP_HTTP_RECOVER_PANICS"); ok {
		if v, ok := parseBoolFlag(raw); ok {
			opts.RecoverPanics = v
		}
	}
	if raw, ok := os.LookupEnv("SLOGCP_HTTP_TRUST_PROXY_HEADERS"); ok {
		if v, ok := parseBoolFlag(raw); ok {
			opts.TrustProxyHeaders = v
		}
	}
	if raw, ok := os.LookupEnv("SLOGCP_HTTP_ATTACH_LOGGER"); ok {
		if v, ok := parseBoolFlag(raw); ok {
			opts.AttachLogger = v
		}
	}
	if raw, ok := os.LookupEnv("SLOGCP_HTTP_START_SPAN_IF_ABSENT"); ok {
		if v, ok := parseBoolFlag(raw); ok {
			opts.StartSpanIfAbsent = v
		}
	}
	if cfg, _ := chatter.LoadConfigFromEnv(os.LookupEnv); true {
		opts.ChatterConfig = cfg.Clone()
	}
	if raw, ok := os.LookupEnv("SLOGCP_HTTP_TRACE_PROJECT_ID"); ok {
		opts.TraceProjectID = strings.TrimSpace(raw)
	}
	if raw, ok := os.LookupEnv("SLOGCP_HTTP_ROUTE_HEADER"); ok {
		header := textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(raw))
		if header != "" {
			opts.RouteGetter = func(r *http.Request) string {
				return r.Header.Get(header)
			}
		}
	}
	if raw, ok := os.LookupEnv("SLOGCP_HTTP_ROUTE_FROM_PATH"); ok {
		if v, ok := parseBoolFlag(raw); ok && v {
			opts.RouteGetter = func(r *http.Request) string {
				if r.URL == nil {
					return ""
				}
				return r.URL.Path
			}
		}
	}

	return opts
}

// WithShouldLog configures a predicate to determine if a request should emit a log entry.
func WithShouldLog(fn ShouldLogFunc) Option {
	return func(o *MiddlewareOptions) {
		o.ShouldLog = fn
	}
}

// WithSkipPathSubstrings configures substring filters applied to the request path.
func WithSkipPathSubstrings(substrings ...string) Option {
	cleaned := make([]string, 0, len(substrings))
	for _, value := range substrings {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		cleaned = append(cleaned, value)
	}
	return func(o *MiddlewareOptions) {
		o.SkipPathSubstrings = cleaned
	}
}

// WithSuppressUnsampledBelow drops logs for unsampled traces below the provided level.
// Responses with HTTP 5xx status codes are always logged so that server errors
// remain observable.
func WithSuppressUnsampledBelow(level slog.Leveler) Option {
	return func(o *MiddlewareOptions) {
		if level == nil {
			o.SuppressUnsampledBelow = nil
			return
		}
		lvl := level.Level()
		o.SuppressUnsampledBelow = &lvl
	}
}

// WithLogRequestHeaderKeys captures the provided request header keys.
func WithLogRequestHeaderKeys(keys ...string) Option {
	cleaned := cleanHeaderKeys(keys)
	return func(o *MiddlewareOptions) {
		o.LogRequestHeaderKeys = cleaned
	}
}

// WithRequestLogging toggles request logging regardless of environment defaults.
func WithRequestLogging(enabled bool) Option {
	return func(o *MiddlewareOptions) {
		o.LogRequests = enabled
	}
}

// WithLogResponseHeaderKeys captures the provided response header keys.
func WithLogResponseHeaderKeys(keys ...string) Option {
	cleaned := cleanHeaderKeys(keys)
	return func(o *MiddlewareOptions) {
		o.LogResponseHeaderKeys = cleaned
	}
}

// WithRequestBodyLimit captures up to limit bytes of the request body.
func WithRequestBodyLimit(limit int64) Option {
	if limit < 0 {
		limit = 0
	}
	return func(o *MiddlewareOptions) {
		o.RequestBodyLimit = limit
	}
}

// WithResponseBodyLimit captures up to limit bytes of the response body.
func WithResponseBodyLimit(limit int64) Option {
	if limit < 0 {
		limit = 0
	}
	return func(o *MiddlewareOptions) {
		o.ResponseBodyLimit = limit
	}
}

// WithRecoverPanics enables panic recovery.
func WithRecoverPanics(recover bool) Option {
	return func(o *MiddlewareOptions) {
		o.RecoverPanics = recover
	}
}

// WithTrustProxyHeaders toggles proxy header trust for remote IP extraction.
func WithTrustProxyHeaders(trust bool) Option {
	return func(o *MiddlewareOptions) {
		o.TrustProxyHeaders = trust
	}
}

// WithContextLogger controls whether the middleware creates a request-scoped
// logger and stores it in the request context via slogcp.ContextWithLogger.
// Default is true.
func WithContextLogger(enabled bool) Option {
	return func(o *MiddlewareOptions) {
		o.AttachLogger = enabled
	}
}

// WithStartSpanIfAbsent controls whether the middleware should start a server
// span when no valid span context is present after propagation. Default is true.
func WithStartSpanIfAbsent(enabled bool) Option {
	return func(o *MiddlewareOptions) {
		o.StartSpanIfAbsent = enabled
	}
}

// WithTracer overrides the tracer used when WithStartSpanIfAbsent is enabled.
// When nil, the middleware uses a package-level tracer derived from the global
// provider.
func WithTracer(tr trace.Tracer) Option {
	return func(o *MiddlewareOptions) {
		o.Tracer = tr
	}
}

// WithChatterConfig installs the provided chatter reduction configuration.
func WithChatterConfig(cfg chatter.Config) Option {
	return func(o *MiddlewareOptions) {
		o.ChatterConfig = cfg.Clone()
	}
}

// WithHealthCheckFilter is deprecated; use WithChatterConfig instead.
func WithHealthCheckFilter(cfg chatter.Config) Option {
	return func(o *MiddlewareOptions) {
		o.ChatterConfig = cfg.Clone()
	}
}

// WithTrustProxyEvaluator supplies a predicate that determines on a per-request
// basis whether proxy headers should be trusted. It is evaluated only when
// TrustProxyHeaders is true or the predicate itself returns true.
func WithTrustProxyEvaluator(fn func(*http.Request) bool) Option {
	return func(o *MiddlewareOptions) {
		o.TrustProxyDecisionFunc = fn
	}
}

// WithTraceProjectID sets the project ID used when decorating request-scoped
// loggers with Cloud Trace aware fields.
func WithTraceProjectID(projectID string) Option {
	return func(o *MiddlewareOptions) {
		o.TraceProjectID = strings.TrimSpace(projectID)
	}
}

// WithRouteGetter installs a function that returns the route template to log
// for the request-scoped logger when context logging is enabled.
func WithRouteGetter(getter func(*http.Request) string) Option {
	return func(o *MiddlewareOptions) {
		o.RouteGetter = getter
	}
}

// WithRouteFromHeader uses the specified header to supply the route template
// when attaching request-scoped loggers.
func WithRouteFromHeader(header string) Option {
	header = textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(header))
	if header == "" {
		header = ""
	}
	return func(o *MiddlewareOptions) {
		if header == "" {
			o.RouteGetter = nil
			return
		}
		o.RouteGetter = func(r *http.Request) string {
			return r.Header.Get(header)
		}
	}
}

// WithRouteFromPath configures the middleware to use the request URL path as
// the route value when attaching request-scoped loggers.
func WithRouteFromPath() Option {
	return func(o *MiddlewareOptions) {
		o.RouteGetter = func(r *http.Request) string {
			if r.URL == nil {
				return ""
			}
			return r.URL.Path
		}
	}
}

// splitAndClean normalizes comma-separated configuration strings into a slice
// of trimmed, non-empty values.
func splitAndClean(input string) []string {
	parts := strings.Split(input, ",")
	cleaned := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		cleaned = append(cleaned, part)
	}
	return cleaned
}

// cleanHeaderKeys canonicalizes and filters header keys, matching the
// behaviour of the net/http package when normalizing request and response
// headers.
func cleanHeaderKeys(keys []string) []string {
	cleaned := make([]string, 0, len(keys))
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		cleaned = append(cleaned, textproto.CanonicalMIMEHeaderKey(key))
	}
	return cleaned
}

// parseLevel converts textual severities into slog.Level values compatible
// with the extended set used throughout slogcp. Google Cloud specific levels
// map to their documented offsets so the middleware compares levels using the
// same ordering as the rest of the project.
func parseLevel(raw string) (slog.Level, error) {
	raw = strings.TrimSpace(strings.ToUpper(raw))
	switch raw {
	case "DEFAULT":
		return slog.Level(30), nil
	case "":
		return 0, strconv.ErrSyntax
	case "DEBUG":
		return slog.LevelDebug, nil
	case "INFO":
		return slog.LevelInfo, nil
	case "NOTICE":
		return slog.Level(2), nil
	case "WARN", "WARNING":
		return slog.LevelWarn, nil
	case "ERROR":
		return slog.LevelError, nil
	case "CRITICAL":
		return slog.Level(12), nil
	case "ALERT":
		return slog.Level(16), nil
	case "EMERGENCY":
		return slog.Level(20), nil
	default:
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			return slog.Level(n), nil
		}
	}
	return 0, strconv.ErrSyntax
}

// parsePositiveInt64 parses strings into non-negative sizes. Negative values
// are clamped to zero so callers can treat them as disabled limits.
func parsePositiveInt64(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, strconv.ErrSyntax
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, err
	}
	if v < 0 {
		v = 0
	}
	return v, nil
}

// parseBoolFlag converts common boolean strings into a bool. Supported values
// are true/false, yes/no, 1/0, and on/off (case-insensitive). The second return
// value reports whether the input matched one of the supported values so callers
// can ignore invalid inputs without overriding defaults.
func parseBoolFlag(raw string) (bool, bool) {
	trimmed := strings.ToLower(strings.TrimSpace(raw))
	switch trimmed {
	case "true", "1", "yes", "on":
		return true, true
	case "false", "0", "no", "off":
		return false, true
	default:
		return false, false
	}
}
