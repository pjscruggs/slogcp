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

package slogcppubsub

import (
	"context"
	"log/slog"
	"strings"

	"cloud.google.com/go/pubsub/v2"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

const defaultConsumerSpanName = "pubsub.process"

// AttrEnricher can append additional attributes to the derived message logger.
type AttrEnricher func(context.Context, *pubsub.Message, *MessageInfo) []slog.Attr

// AttrTransformer can mutate or redact the attribute slice before it is applied
// to the derived message logger.
type AttrTransformer func(context.Context, []slog.Attr, *pubsub.Message, *MessageInfo) []slog.Attr

// Option configures slogcppubsub behavior.
type Option func(*config)

type config struct {
	logger                              *slog.Logger
	projectID                           string
	subscriptionID                      string
	topicID                             string
	enableOTel                          bool
	spanStrategy                        SpanStrategy
	tracerProvider                      trace.TracerProvider
	publicEndpoint                      bool
	publicEndpointCorrelateLogsToRemote bool
	propagators                         propagation.TextMapPropagator
	propagatorsSet                      bool
	propagateTrace                      bool
	propagateBaggage                    bool
	caseInsensitiveExtraction           bool
	injectOnlyIfSpanPresent             bool
	spanName                            string
	spanAttributes                      []attribute.KeyValue
	logMessageID                        bool
	logOrderingKey                      bool
	logDeliveryAttempt                  bool
	logPublishTime                      bool
	attrEnrichers                       []AttrEnricher
	attrTransformers                    []AttrTransformer
	googClientExtraction                bool
	googClientInjection                 bool
}

// defaultConfig returns a config populated with production-oriented defaults.
func defaultConfig() *config {
	return &config{
		logger:             slog.Default(),
		enableOTel:         true,
		spanStrategy:       SpanStrategyAlways,
		propagateTrace:     true,
		propagateBaggage:   false,
		spanName:           defaultConsumerSpanName,
		logMessageID:       false,
		logOrderingKey:     false,
		logDeliveryAttempt: true,
		logPublishTime:     false,
	}
}

// applyOptions applies Option values to the default configuration.
func applyOptions(opts []Option) *config {
	cfg := defaultConfig()
	for _, opt := range opts {
		if opt != nil {
			opt(cfg)
		}
	}
	return cfg
}

// WithLogger sets the base logger used to derive per-message loggers.
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

// WithSubscription sets the subscription identifier used for span and logger
// enrichment.
func WithSubscription(sub *pubsub.Subscriber) Option {
	return func(cfg *config) {
		if sub == nil {
			return
		}
		cfg.subscriptionID = strings.TrimSpace(sub.ID())
	}
}

// WithSubscriptionID sets the subscription identifier used for span and logger
// enrichment.
func WithSubscriptionID(subscriptionID string) Option {
	return func(cfg *config) {
		cfg.subscriptionID = strings.TrimSpace(subscriptionID)
	}
}

// WithTopic sets the topic identifier used for span and logger enrichment.
func WithTopic(topic *pubsub.Publisher) Option {
	return func(cfg *config) {
		if topic == nil {
			return
		}
		cfg.topicID = strings.TrimSpace(topic.ID())
	}
}

// WithTopicID sets the topic identifier used for span and logger enrichment.
func WithTopicID(topicID string) Option {
	return func(cfg *config) {
		cfg.topicID = strings.TrimSpace(topicID)
	}
}

// WithOTel enables or disables creation of an application-level consumer span
// around the user handler. When disabled, slogcppubsub still extracts trace
// context (when enabled) so logs can correlate to the upstream trace.
func WithOTel(enabled bool) Option {
	return func(cfg *config) {
		cfg.enableOTel = enabled
	}
}

// SpanStrategy controls when slogcppubsub starts an application-level span
// around message processing.
type SpanStrategy int

const (
	// SpanStrategyAuto avoids duplicating spans from upstream instrumentation by
	// skipping span creation when a local span is already active in the context.
	SpanStrategyAuto SpanStrategy = iota
	// SpanStrategyAlways always starts a span when OTel is enabled, even when a
	// local span is already active.
	SpanStrategyAlways
)

// WithSpanStrategy configures when slogcppubsub starts message-processing spans.
func WithSpanStrategy(strategy SpanStrategy) Option {
	return func(cfg *config) {
		cfg.spanStrategy = strategy
	}
}

// WithTracerProvider installs the OpenTelemetry tracer provider used when
// starting consumer spans.
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(cfg *config) {
		cfg.tracerProvider = tp
	}
}

// WithPublicEndpoint toggles the trust boundary behavior: when enabled,
// slogcppubsub starts a new root trace and links the extracted remote context
// rather than parenting spans directly.
func WithPublicEndpoint(enabled bool) Option {
	return func(cfg *config) {
		cfg.publicEndpoint = enabled
	}
}

// WithPublicEndpointCorrelateLogsToRemote controls whether logs are correlated
// to extracted remote trace context when public endpoint mode is enabled and no
// local span is created (for example, when WithOTel(false)). The default is
// false so untrusted producers cannot choose the trace associated with logs.
func WithPublicEndpointCorrelateLogsToRemote(enabled bool) Option {
	return func(cfg *config) {
		cfg.publicEndpointCorrelateLogsToRemote = enabled
	}
}

// WithPropagators supplies a TextMapPropagator used for extracting (subscriber)
// or injecting (publisher) trace context. When omitted, otel.GetTextMapPropagator()
// is used. Passing nil is treated the same as omitting the option.
func WithPropagators(p propagation.TextMapPropagator) Option {
	return func(cfg *config) {
		cfg.propagators = p
		cfg.propagatorsSet = p != nil
	}
}

// WithTracePropagation toggles extraction and injection of trace context.
// Enabled by default.
func WithTracePropagation(enabled bool) Option {
	return func(cfg *config) {
		cfg.propagateTrace = enabled
	}
}

// WithBaggagePropagation controls whether baggage is injected and extracted via
// message attributes when the selected propagator supports it. Disabled by
// default because baggage can carry sensitive/high-cardinality data and Pub/Sub
// message attributes are size-limited.
func WithBaggagePropagation(enabled bool) Option {
	return func(cfg *config) {
		cfg.propagateBaggage = enabled
	}
}

// WithCaseInsensitiveExtraction controls whether trace context extraction
// treats propagation keys as case-insensitive. Disabled by default because
// Pub/Sub attributes are case-sensitive.
func WithCaseInsensitiveExtraction(enabled bool) Option {
	return func(cfg *config) {
		cfg.caseInsensitiveExtraction = enabled
	}
}

// WithInjectOnlyIfSpanPresent restores the legacy injection behavior of only
// emitting trace context when a valid span context is present in ctx. The
// default behavior is to always invoke the propagator so baggage and other
// signal types can flow when configured.
func WithInjectOnlyIfSpanPresent(enabled bool) Option {
	return func(cfg *config) {
		cfg.injectOnlyIfSpanPresent = enabled
	}
}

// WithSpanName overrides the span name used when creating consumer spans.
func WithSpanName(name string) Option {
	return func(cfg *config) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		cfg.spanName = name
	}
}

// WithSpanAttributes appends OpenTelemetry span attributes applied when
// slogcppubsub creates a consumer span.
func WithSpanAttributes(attrs ...attribute.KeyValue) Option {
	return func(cfg *config) {
		cfg.spanAttributes = append(cfg.spanAttributes, attrs...)
	}
}

// WithLogMessageID controls whether derived loggers include the message ID.
// Disabled by default because message IDs are high cardinality.
func WithLogMessageID(enabled bool) Option {
	return func(cfg *config) {
		cfg.logMessageID = enabled
	}
}

// WithLogOrderingKey controls whether derived loggers include the Pub/Sub
// ordering key. Disabled by default because ordering keys can be high
// cardinality.
func WithLogOrderingKey(enabled bool) Option {
	return func(cfg *config) {
		cfg.logOrderingKey = enabled
	}
}

// WithLogDeliveryAttempt controls whether derived loggers include the delivery
// attempt count when known. Enabled by default.
func WithLogDeliveryAttempt(enabled bool) Option {
	return func(cfg *config) {
		cfg.logDeliveryAttempt = enabled
	}
}

// WithLogPublishTime controls whether derived loggers include the message
// publish timestamp. Disabled by default.
func WithLogPublishTime(enabled bool) Option {
	return func(cfg *config) {
		cfg.logPublishTime = enabled
	}
}

// WithAttrEnricher registers a callback that can add attributes to the derived
// message logger.
func WithAttrEnricher(enricher AttrEnricher) Option {
	return func(cfg *config) {
		if enricher != nil {
			cfg.attrEnrichers = append(cfg.attrEnrichers, enricher)
		}
	}
}

// WithAttrTransformer registers a callback that can transform or redact the
// attribute slice before it is applied to the derived message logger.
func WithAttrTransformer(transformer AttrTransformer) Option {
	return func(cfg *config) {
		if transformer != nil {
			cfg.attrTransformers = append(cfg.attrTransformers, transformer)
		}
	}
}

// WithGoogClientCompat enables interop with the Go Pub/Sub client's prefixed
// trace attribute keys (for example `googclient_traceparent`). It enables both
// extraction fallback and optional injection of those keys.
func WithGoogClientCompat(enabled bool) Option {
	return func(cfg *config) {
		cfg.googClientExtraction = enabled
		cfg.googClientInjection = enabled
	}
}

// WithGoogClientExtraction toggles extraction from `googclient_`-prefixed
// keys when standard W3C keys are absent.
func WithGoogClientExtraction(enabled bool) Option {
	return func(cfg *config) {
		cfg.googClientExtraction = enabled
	}
}

// WithGoogClientInjection toggles injection of `googclient_`-prefixed keys in
// addition to standard W3C keys.
func WithGoogClientInjection(enabled bool) Option {
	return func(cfg *config) {
		cfg.googClientInjection = enabled
	}
}
