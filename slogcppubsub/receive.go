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
	"fmt"
	"log/slog"
	"strings"
	"time"

	"cloud.google.com/go/pubsub/v2"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/pjscruggs/slogcp"
)

const instrumentationName = "github.com/pjscruggs/slogcp/slogcppubsub"

type messageInfoKey struct{}

// MessageInfo captures message metadata surfaced to handlers via context.
type MessageInfo struct {
	SubscriptionID string
	TopicID        string

	MessageID        string
	OrderingKey      string
	PublishTime      time.Time
	deliveryAttempt  int
	hasDeliveryCount bool

	RemoteTraceparent string
	RemoteTraceID     string
	RemoteSpanID      string
	RemoteSampled     bool
}

// InfoFromContext retrieves MessageInfo attached by WrapReceiveHandler.
func InfoFromContext(ctx context.Context) (*MessageInfo, bool) {
	if ctx == nil {
		return nil, false
	}
	info, ok := ctx.Value(messageInfoKey{}).(*MessageInfo)
	return info, ok && info != nil
}

// DeliveryAttempt returns the delivery attempt count and whether it is known.
func (mi *MessageInfo) DeliveryAttempt() (int, bool) {
	if mi == nil {
		return 0, false
	}
	return mi.deliveryAttempt, mi.hasDeliveryCount
}

// WrapReceiveHandler wraps a pubsub.Subscription.Receive callback with trace
// extraction, optional consumer span creation, and message-scoped logger
// enrichment.
func WrapReceiveHandler(handler func(context.Context, *pubsub.Message), opts ...Option) func(context.Context, *pubsub.Message) {
	cfg := applyOptions(opts)
	projectID := resolveProjectID(cfg.projectID)
	tracer := otel.Tracer(instrumentationName)
	if cfg.tracerProvider != nil {
		tracer = cfg.tracerProvider.Tracer(instrumentationName)
	}

	return func(ctx context.Context, msg *pubsub.Message) {
		if handler == nil {
			return
		}

		messageAttrs := msgAttributes(msg)

		var extracted trace.SpanContext
		if cfg.publicEndpoint {
			extractedCtx, remote := ensureSpanContext(ctx, messageAttrs, cfg)
			extracted = remote
			if !cfg.enableOTel && cfg.publicEndpointCorrelateLogsToRemote {
				ctx = extractedCtx
			}
		} else {
			ctx, extracted = ensureSpanContext(ctx, messageAttrs, cfg)
		}

		info := newMessageInfo(cfg, msg, extracted)
		ctx = context.WithValue(ctx, messageInfoKey{}, info)

		ctx, endSpan := startConsumerSpan(ctx, extracted, tracer, projectID, cfg, info, msg)
		defer endSpan()

		traceAttrs, _ := slogcp.TraceAttributes(ctx, projectID)
		attrs := info.loggerAttrs(cfg, msg, traceAttrs)
		attrs = applyEnrichers(ctx, cfg, attrs, msg, info)
		attrs = applyTransformers(ctx, cfg, attrs, msg, info)

		baseLogger := cfg.logger
		if baseLogger == nil {
			baseLogger = slogcp.Logger(ctx)
		}
		requestLogger := loggerWithAttrs(baseLogger, attrs)
		ctx = slogcp.ContextWithLogger(ctx, requestLogger)

		handler(ctx, msg)
	}
}

// msgAttributes returns the message attributes map, or nil when msg is nil.
func msgAttributes(msg *pubsub.Message) map[string]string {
	if msg == nil {
		return nil
	}
	return msg.Attributes
}

// resolveProjectID returns the configured project ID or derives it from runtime detection.
func resolveProjectID(explicit string) string {
	explicit = strings.TrimSpace(explicit)
	if explicit != "" {
		return explicit
	}
	return strings.TrimSpace(slogcp.DetectRuntimeInfo().ProjectID)
}

// newMessageInfo snapshots message metadata and extracted remote trace context.
func newMessageInfo(cfg *config, msg *pubsub.Message, extracted trace.SpanContext) *MessageInfo {
	info := &MessageInfo{}
	if cfg != nil {
		info.SubscriptionID = strings.TrimSpace(cfg.subscriptionID)
		info.TopicID = strings.TrimSpace(cfg.topicID)
	}
	if extracted.IsValid() && extracted.IsRemote() {
		info.RemoteTraceparent = formatTraceparent(extracted)
		info.RemoteTraceID = extracted.TraceID().String()
		info.RemoteSpanID = extracted.SpanID().String()
		info.RemoteSampled = extracted.IsSampled()
	}
	if msg == nil {
		return info
	}

	info.MessageID = msg.ID
	info.OrderingKey = msg.OrderingKey
	info.PublishTime = msg.PublishTime
	if msg.DeliveryAttempt != nil {
		info.deliveryAttempt = *msg.DeliveryAttempt
		info.hasDeliveryCount = true
	}
	return info
}

// startConsumerSpan starts an optional application-level consumer span and returns a span end callback.
func startConsumerSpan(
	ctx context.Context,
	extracted trace.SpanContext,
	tracer trace.Tracer,
	projectID string,
	cfg *config,
	info *MessageInfo,
	msg *pubsub.Message,
) (context.Context, func()) {
	if cfg == nil {
		cfg = defaultConfig()
	}
	if !cfg.enableOTel {
		return ctx, func() {}
	}

	if cfg.spanStrategy == SpanStrategyAuto && !cfg.publicEndpoint {
		current := trace.SpanContextFromContext(ctx)
		if current.IsValid() && !current.IsRemote() {
			return ctx, func() {}
		}
	}

	spanOpts := consumerSpanOptions(projectID, cfg, extracted, info, msg)
	resolvedSpanName := resolveSpanName(cfg.spanName)
	ctx, span := tracer.Start(ctx, resolvedSpanName, spanOpts...)
	return ctx, func() { span.End() }
}

// resolveSpanName returns a usable span name, falling back to a package default.
func resolveSpanName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return defaultConsumerSpanName
	}
	return name
}

// consumerSpanOptions builds the span start options for the consumer span.
func consumerSpanOptions(projectID string, cfg *config, extracted trace.SpanContext, info *MessageInfo, msg *pubsub.Message) []trace.SpanStartOption {
	opts := []trace.SpanStartOption{
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(consumerSpanAttrs(projectID, cfg, info, msg)...),
	}
	if !cfg.publicEndpoint {
		return opts
	}

	opts = append(opts, trace.WithNewRoot())
	if extracted.IsValid() && extracted.IsRemote() {
		opts = append(opts, trace.WithLinks(trace.Link{SpanContext: extracted}))
	}
	return opts
}

// consumerSpanAttrs returns OpenTelemetry attributes for the consumer span.
func consumerSpanAttrs(projectID string, cfg *config, info *MessageInfo, msg *pubsub.Message) []attribute.KeyValue {
	if cfg == nil {
		cfg = defaultConfig()
	}
	capacity := 10
	if len(cfg.spanAttributes) > 0 {
		capacity += len(cfg.spanAttributes)
	}

	attrs := make([]attribute.KeyValue, 0, capacity)
	attrs = append(attrs, semconv.MessagingSystemGCPPubsub)
	attrs = append(attrs, semconv.MessagingOperationName("process"))
	attrs = append(attrs, semconv.MessagingOperationTypeDeliver)

	attrs = appendConsumerDestinationAttrs(attrs, info)
	attrs = appendConsumerMessageAttrs(attrs, msg)
	attrs = appendProjectSpanAttrs(attrs, projectID)
	if len(cfg.spanAttributes) > 0 {
		attrs = append(attrs, cfg.spanAttributes...)
	}
	return attrs
}

// appendConsumerDestinationAttrs appends destination attributes derived from MessageInfo.
func appendConsumerDestinationAttrs(attrs []attribute.KeyValue, info *MessageInfo) []attribute.KeyValue {
	if info == nil {
		return attrs
	}
	if subID := strings.TrimSpace(info.SubscriptionID); subID != "" {
		attrs = append(attrs, semconv.MessagingDestinationName(subID))
	}
	if topicID := strings.TrimSpace(info.TopicID); topicID != "" {
		attrs = append(attrs, semconv.MessagingDestinationPublishName(topicID))
	}
	return attrs
}

// appendConsumerMessageAttrs appends message attributes derived from pubsub.Message.
func appendConsumerMessageAttrs(attrs []attribute.KeyValue, msg *pubsub.Message) []attribute.KeyValue {
	if msg == nil {
		return attrs
	}
	if msg.ID != "" {
		attrs = append(attrs, semconv.MessagingMessageID(msg.ID))
	}
	if len(msg.Data) > 0 {
		attrs = append(attrs, semconv.MessagingMessageBodySize(len(msg.Data)))
	}
	if msg.OrderingKey != "" {
		attrs = append(attrs, semconv.MessagingGCPPubsubMessageOrderingKey(msg.OrderingKey))
	}
	if msg.DeliveryAttempt != nil {
		attrs = append(attrs, semconv.MessagingGCPPubsubMessageDeliveryAttempt(*msg.DeliveryAttempt))
	}
	return attrs
}

// appendProjectSpanAttrs appends project ID attributes when available.
func appendProjectSpanAttrs(attrs []attribute.KeyValue, projectID string) []attribute.KeyValue {
	if projectID == "" {
		return attrs
	}
	return append(attrs, attribute.String("gcp.project_id", projectID))
}

// applyEnrichers appends slog attributes produced by configured enrichers.
func applyEnrichers(ctx context.Context, cfg *config, attrs []slog.Attr, msg *pubsub.Message, info *MessageInfo) []slog.Attr {
	if cfg == nil {
		return attrs
	}
	for _, enricher := range cfg.attrEnrichers {
		if enricher == nil {
			continue
		}
		if extra := enricher(ctx, msg, info); len(extra) > 0 {
			attrs = append(attrs, extra...)
		}
	}
	return attrs
}

// applyTransformers applies configured transformations to the slog attribute slice.
func applyTransformers(ctx context.Context, cfg *config, attrs []slog.Attr, msg *pubsub.Message, info *MessageInfo) []slog.Attr {
	if cfg == nil {
		return attrs
	}
	for _, transformer := range cfg.attrTransformers {
		if transformer == nil {
			continue
		}
		attrs = transformer(ctx, attrs, msg, info)
	}
	return attrs
}

// loggerAttrs assembles the structured log attributes for a message.
func (mi *MessageInfo) loggerAttrs(cfg *config, msg *pubsub.Message, traceAttrs []slog.Attr) []slog.Attr {
	if cfg == nil {
		cfg = defaultConfig()
	}

	attrs := make([]slog.Attr, 0, len(traceAttrs)+10)
	attrs = append(attrs, traceAttrs...)
	attrs = appendRemoteTraceparent(attrs, cfg, mi)
	attrs = appendMessagingCoreLoggerAttrs(attrs)
	attrs = appendMessagingDestinationLoggerAttrs(attrs, mi)
	attrs = appendMessagingMessageLoggerAttrs(attrs, cfg, msg)
	return attrs
}

// appendRemoteTraceparent appends a debug-only traceparent field for public endpoint mode.
func appendRemoteTraceparent(attrs []slog.Attr, cfg *config, mi *MessageInfo) []slog.Attr {
	if !cfg.publicEndpoint {
		return attrs
	}
	if mi == nil || mi.RemoteTraceparent == "" {
		return attrs
	}
	return append(attrs, slog.String("pubsub.remote.traceparent", mi.RemoteTraceparent))
}

// appendMessagingCoreLoggerAttrs appends core messaging semantic convention fields.
func appendMessagingCoreLoggerAttrs(attrs []slog.Attr) []slog.Attr {
	attrs = append(attrs, slog.String(string(semconv.MessagingSystemGCPPubsub.Key), semconv.MessagingSystemGCPPubsub.Value.AsString()))
	attrs = append(attrs, slog.String(string(semconv.MessagingOperationNameKey), "process"))
	return append(attrs, slog.String(string(semconv.MessagingOperationTypeDeliver.Key), semconv.MessagingOperationTypeDeliver.Value.AsString()))
}

// appendMessagingDestinationLoggerAttrs appends destination fields from MessageInfo.
func appendMessagingDestinationLoggerAttrs(attrs []slog.Attr, mi *MessageInfo) []slog.Attr {
	if mi == nil {
		return attrs
	}
	if subID := strings.TrimSpace(mi.SubscriptionID); subID != "" {
		attrs = append(attrs, slog.String(string(semconv.MessagingDestinationNameKey), subID))
	}
	if topicID := strings.TrimSpace(mi.TopicID); topicID != "" {
		attrs = append(attrs, slog.String(string(semconv.MessagingDestinationPublishNameKey), topicID))
	}
	return attrs
}

// appendMessagingMessageLoggerAttrs appends message-specific fields according to configuration.
func appendMessagingMessageLoggerAttrs(attrs []slog.Attr, cfg *config, msg *pubsub.Message) []slog.Attr {
	if msg == nil {
		return attrs
	}
	attrs = appendMessageIDLoggerAttr(attrs, cfg, msg)
	attrs = appendMessageBodySizeLoggerAttr(attrs, msg)
	attrs = appendOrderingKeyLoggerAttr(attrs, cfg, msg)
	attrs = appendDeliveryAttemptLoggerAttr(attrs, cfg, msg)
	return appendPublishTimeLoggerAttr(attrs, cfg, msg)
}

// appendMessageIDLoggerAttr appends a message ID field when enabled.
func appendMessageIDLoggerAttr(attrs []slog.Attr, cfg *config, msg *pubsub.Message) []slog.Attr {
	if !cfg.logMessageID || msg.ID == "" {
		return attrs
	}
	return append(attrs, slog.String(string(semconv.MessagingMessageIDKey), msg.ID))
}

// appendMessageBodySizeLoggerAttr appends a body size field when the message has a non-empty payload.
func appendMessageBodySizeLoggerAttr(attrs []slog.Attr, msg *pubsub.Message) []slog.Attr {
	if len(msg.Data) == 0 {
		return attrs
	}
	bodySizeKV := semconv.MessagingMessageBodySize(len(msg.Data))
	return append(attrs, slog.Int(string(bodySizeKV.Key), int(bodySizeKV.Value.AsInt64())))
}

// appendOrderingKeyLoggerAttr appends an ordering key field when enabled.
func appendOrderingKeyLoggerAttr(attrs []slog.Attr, cfg *config, msg *pubsub.Message) []slog.Attr {
	if !cfg.logOrderingKey || msg.OrderingKey == "" {
		return attrs
	}
	orderKV := semconv.MessagingGCPPubsubMessageOrderingKey(msg.OrderingKey)
	return append(attrs, slog.String(string(orderKV.Key), orderKV.Value.AsString()))
}

// appendDeliveryAttemptLoggerAttr appends a delivery attempt field when enabled and present.
func appendDeliveryAttemptLoggerAttr(attrs []slog.Attr, cfg *config, msg *pubsub.Message) []slog.Attr {
	if !cfg.logDeliveryAttempt || msg.DeliveryAttempt == nil {
		return attrs
	}
	attemptKV := semconv.MessagingGCPPubsubMessageDeliveryAttempt(*msg.DeliveryAttempt)
	return append(attrs, slog.Int(string(attemptKV.Key), int(attemptKV.Value.AsInt64())))
}

// appendPublishTimeLoggerAttr appends a publish timestamp field when enabled.
func appendPublishTimeLoggerAttr(attrs []slog.Attr, cfg *config, msg *pubsub.Message) []slog.Attr {
	if !cfg.logPublishTime || msg.PublishTime.IsZero() {
		return attrs
	}
	return append(attrs, slog.Time("pubsub.message.publish_time", msg.PublishTime))
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

// formatTraceparent formats a SpanContext as a W3C traceparent header value.
func formatTraceparent(sc trace.SpanContext) string {
	if !sc.IsValid() {
		return ""
	}
	return fmt.Sprintf("00-%s-%s-%02x", sc.TraceID().String(), sc.SpanID().String(), byte(sc.TraceFlags()))
}
