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

package slogcpgrpc

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// AttrEnricher can append additional attributes to the request-scoped logger.
// It receives the active context and mutable RequestInfo structure.
type AttrEnricher func(ctx context.Context, info *RequestInfo) []slog.Attr

// AttrTransformer can mutate or redact attributes before they are applied to
// the derived logger.
type AttrTransformer func(ctx context.Context, attrs []slog.Attr, info *RequestInfo) []slog.Attr

// Option configures gRPC interceptors and helper functions.
type Option func(*config)

type config struct {
	logger           *slog.Logger
	projectID        string
	enableOTel       bool
	tracerProvider   trace.TracerProvider
	propagators      propagation.TextMapPropagator
	propagatorsSet   bool
	propagateTrace   bool
	publicEndpoint   bool
	spanAttributes   []attribute.KeyValue
	filters          []otelgrpc.Filter
	attrEnrichers    []AttrEnricher
	attrTransformers []AttrTransformer
	includePeer      bool
	includeSizes     bool
	injectLegacyXCTC bool
}

// defaultConfig returns the baseline configuration for slogcp gRPC helpers.
func defaultConfig() *config {
	return &config{
		logger:         slog.Default(),
		enableOTel:     true,
		includePeer:    true,
		includeSizes:   true,
		propagateTrace: true,
	}
}

// applyOptions applies the provided Option list, starting from defaultConfig.
func applyOptions(opts []Option) *config {
	cfg := defaultConfig()
	for _, opt := range opts {
		if opt != nil {
			opt(cfg)
		}
	}
	return cfg
}

// WithLogger overrides the base logger used to derive per-RPC loggers.
func WithLogger(logger *slog.Logger) Option {
	return func(cfg *config) {
		if logger == nil {
			cfg.logger = slog.Default()
			return
		}
		cfg.logger = logger
	}
}

// WithProjectID overrides the Cloud project used when formatting trace
// correlation fields.
func WithProjectID(projectID string) Option {
	return func(cfg *config) {
		cfg.projectID = projectID
	}
}

// WithPropagators sets the text map propagator used for extracting metadata
// (server) or injecting metadata (client). When omitted, the global propagator
// is used.
func WithPropagators(p propagation.TextMapPropagator) Option {
	return func(cfg *config) {
		cfg.propagators = p
		cfg.propagatorsSet = true
	}
}

// WithTracerProvider configures the tracer provider used when composing
// otelgrpc StatsHandlers.
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(cfg *config) {
		cfg.tracerProvider = tp
	}
}

// WithTracePropagation toggles extraction and injection of trace context on
// gRPC servers and clients. Enabled by default.
func WithTracePropagation(enabled bool) Option {
	return func(cfg *config) {
		cfg.propagateTrace = enabled
	}
}

// WithPublicEndpoint toggles otelgrpc's public endpoint hint.
func WithPublicEndpoint(enabled bool) Option {
	return func(cfg *config) {
		cfg.publicEndpoint = enabled
	}
}

// WithOTel enables or disables automatic otelgrpc StatsHandlers. Enabled by
// default.
func WithOTel(enabled bool) Option {
	return func(cfg *config) {
		cfg.enableOTel = enabled
	}
}

// WithSpanAttributes appends OpenTelemetry span attributes applied when
// otelgrpc instrumentation is active.
func WithSpanAttributes(attrs ...attribute.KeyValue) Option {
	return func(cfg *config) {
		cfg.spanAttributes = append(cfg.spanAttributes, attrs...)
	}
}

// WithFilter appends an otelgrpc filter applied before spans are created.
func WithFilter(filter otelgrpc.Filter) Option {
	return func(cfg *config) {
		if filter != nil {
			cfg.filters = append(cfg.filters, filter)
		}
	}
}

// WithAttrEnricher registers a callback for adding attributes to derived
// loggers.
func WithAttrEnricher(enricher AttrEnricher) Option {
	return func(cfg *config) {
		if enricher != nil {
			cfg.attrEnrichers = append(cfg.attrEnrichers, enricher)
		}
	}
}

// WithAttrTransformer registers a callback for mutating attributes before they
// are applied to derived loggers.
func WithAttrTransformer(transformer AttrTransformer) Option {
	return func(cfg *config) {
		if transformer != nil {
			cfg.attrTransformers = append(cfg.attrTransformers, transformer)
		}
	}
}

// WithPeerInfo toggles inclusion of peer address attributes. Enabled by default.
func WithPeerInfo(enabled bool) Option {
	return func(cfg *config) {
		cfg.includePeer = enabled
	}
}

// WithPayloadSizes toggles inclusion of request/response message size
// attributes. Enabled by default.
func WithPayloadSizes(enabled bool) Option {
	return func(cfg *config) {
		cfg.includeSizes = enabled
	}
}

// WithLegacyXCloudInjection toggles synthesis of the legacy X-Cloud-Trace-Context
// metadata on client RPCs.
func WithLegacyXCloudInjection(enabled bool) Option {
	return func(cfg *config) {
		cfg.injectLegacyXCTC = enabled
	}
}
