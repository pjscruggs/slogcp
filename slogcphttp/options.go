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
	"log/slog"
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// AttrEnricher can append additional attributes to the request-scoped logger.
// Implementations may inspect the incoming request or the mutable *RequestScope
// to add custom fields. Returned attributes are appended in call order.
type AttrEnricher func(*http.Request, *RequestScope) []slog.Attr

// AttrTransformer can modify or redact the attribute slice produced by the
// middleware before it is applied to the derived logger. Transformers run after
// AttrEnrichers and see the accumulated slice.
type AttrTransformer func([]slog.Attr, *http.Request, *RequestScope) []slog.Attr

// Option configures HTTP middleware or transport behaviour.
type Option func(*config)

type config struct {
	logger                 *slog.Logger
	projectID              string
	enableOTel             bool
	tracerProvider         trace.TracerProvider
	propagators            propagation.TextMapPropagator
	propagatorsSet         bool
	propagateTrace         bool
	publicEndpoint         bool
	spanNameFormatter      func(string, *http.Request) string
	filters                []otelhttp.Filter
	attrEnrichers          []AttrEnricher
	attrTransformers       []AttrTransformer
	routeGetter            func(*http.Request) string
	includeClientIP        bool
	includeQuery           bool
	includeUserAgent       bool
	injectLegacyXCTC       bool
	includeHTTPRequestAttr bool
}

// defaultConfig returns the baseline configuration for slogcp HTTP helpers.
func defaultConfig() *config {
	return &config{
		logger:          slog.Default(),
		enableOTel:      true,
		includeClientIP: true,
		propagateTrace:  true,
	}
}

// applyOptions applies the provided options on top of defaultConfig.
func applyOptions(opts []Option) *config {
	cfg := defaultConfig()
	for _, opt := range opts {
		if opt != nil {
			opt(cfg)
		}
	}
	return cfg
}

// WithLogger sets the base logger used to derive per-request loggers. When
// nil, slog.Default() is used.
func WithLogger(logger *slog.Logger) Option {
	return func(cfg *config) {
		if logger == nil {
			cfg.logger = slog.Default()
			return
		}
		cfg.logger = logger
	}
}

// WithProjectID overrides the project ID used for Cloud Trace correlation
// fields. When unset, the middleware falls back to environment- and metadata-
// derived values.
func WithProjectID(projectID string) Option {
	return func(cfg *config) {
		cfg.projectID = projectID
	}
}

// WithPropagators supplies a TextMapPropagator used for extracting (server) or
// injecting (client) trace context. When omitted, otel.GetTextMapPropagator()
// is used.
func WithPropagators(p propagation.TextMapPropagator) Option {
	return func(cfg *config) {
		cfg.propagators = p
		cfg.propagatorsSet = true
	}
}

// WithTracerProvider installs the OpenTelemetry tracer provider used when
// composing the otelhttp handler.
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(cfg *config) {
		cfg.tracerProvider = tp
	}
}

// WithTracePropagation toggles extraction and injection of trace context on
// HTTP middleware and transports. Enabled by default.
func WithTracePropagation(enabled bool) Option {
	return func(cfg *config) {
		cfg.propagateTrace = enabled
	}
}

// WithPublicEndpoint toggles the otelhttp public endpoint hint.
func WithPublicEndpoint(enabled bool) Option {
	return func(cfg *config) {
		cfg.publicEndpoint = enabled
	}
}

// WithOTel enables or disables automatic otelhttp instrumentation. It is
// enabled by default.
func WithOTel(enabled bool) Option {
	return func(cfg *config) {
		cfg.enableOTel = enabled
	}
}

// WithSpanNameFormatter customizes otelhttp span naming.
func WithSpanNameFormatter(formatter func(string, *http.Request) string) Option {
	return func(cfg *config) {
		cfg.spanNameFormatter = formatter
	}
}

// WithFilter appends an otelhttp filter applied to inbound requests prior to
// span creation.
func WithFilter(filter otelhttp.Filter) Option {
	return func(cfg *config) {
		if filter != nil {
			cfg.filters = append(cfg.filters, filter)
		}
	}
}

// WithAttrEnricher registers a callback that can add attributes to the derived
// logger.
func WithAttrEnricher(enricher AttrEnricher) Option {
	return func(cfg *config) {
		if enricher != nil {
			cfg.attrEnrichers = append(cfg.attrEnrichers, enricher)
		}
	}
}

// WithAttrTransformer registers a callback that can transform or redact the
// attribute slice before it is applied to the request-scoped logger.
func WithAttrTransformer(transformer AttrTransformer) Option {
	return func(cfg *config) {
		if transformer != nil {
			cfg.attrTransformers = append(cfg.attrTransformers, transformer)
		}
	}
}

// WithRouteGetter overrides how the middleware resolves the route template for
// a request (e.g., mux-specific variables).
func WithRouteGetter(fn func(*http.Request) string) Option {
	return func(cfg *config) {
		cfg.routeGetter = fn
	}
}

// WithClientIP toggles inclusion of the client IP attribute on the derived
// logger. The default is true.
func WithClientIP(enabled bool) Option {
	return func(cfg *config) {
		cfg.includeClientIP = enabled
	}
}

// WithIncludeQuery toggles inclusion of the raw query string on the derived
// logger. By default queries are omitted to avoid logging sensitive data.
func WithIncludeQuery(enabled bool) Option {
	return func(cfg *config) {
		cfg.includeQuery = enabled
	}
}

// WithUserAgent toggles inclusion of the User-Agent attribute. This is off by
// default to help avoid logging high-cardinality identifiers.
func WithUserAgent(enabled bool) Option {
	return func(cfg *config) {
		cfg.includeUserAgent = enabled
	}
}

// WithLegacyXCloudInjection toggles synthesis of the legacy X-Cloud-Trace-Context
// header on outbound HTTP client requests managed by Transport. W3C traceparent
// headers remain enabled regardless of this setting.
func WithLegacyXCloudInjection(enabled bool) Option {
	return func(cfg *config) {
		cfg.injectLegacyXCTC = enabled
	}
}

// WithHTTPRequestAttr toggles automatic inclusion of the Cloud Logging
// httpRequest payload on the derived request-scoped logger. The attribute is
// disabled by default so applications opt in explicitly.
func WithHTTPRequestAttr(enabled bool) Option {
	return func(cfg *config) {
		cfg.includeHTTPRequestAttr = enabled
	}
}
