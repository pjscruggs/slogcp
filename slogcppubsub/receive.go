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

	"cloud.google.com/go/pubsub"
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
	tracer := resolveTracer(cfg)

	return func(ctx context.Context, msg *pubsub.Message) {
		if handler == nil {
			return
		}

		messageAttrs := msgAttributes(msg)

		var extracted trace.SpanContext
		if cfg != nil && cfg.publicEndpoint {
			extractedCtx, remote := ensureSpanContext(ctx, messageAttrs, cfg, true)
			extracted = remote
			if !cfg.enableOTel && cfg.publicEndpointCorrelateLogsToRemote {
				ctx = extractedCtx
			}
		} else {
			ctx, extracted = ensureSpanContext(ctx, messageAttrs, cfg, true)
		}

		info := newMessageInfo(cfg, msg, extracted)
		ctx = context.WithValue(ctx, messageInfoKey{}, info)

		ctx, span := startConsumerSpan(ctx, extracted, tracer, projectID, cfg, info, msg)
		if span != nil {
			defer span.End()
		}

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

func msgAttributes(msg *pubsub.Message) map[string]string {
	if msg == nil {
		return nil
	}
	return msg.Attributes
}

func resolveProjectID(explicit string) string {
	explicit = strings.TrimSpace(explicit)
	if explicit != "" {
		return explicit
	}
	return strings.TrimSpace(slogcp.DetectRuntimeInfo().ProjectID)
}

func resolveTracer(cfg *config) trace.Tracer {
	if cfg != nil && cfg.tracerProvider != nil {
		return cfg.tracerProvider.Tracer(instrumentationName)
	}
	return otel.Tracer(instrumentationName)
}

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

func startConsumerSpan(
	ctx context.Context,
	extracted trace.SpanContext,
	tracer trace.Tracer,
	projectID string,
	cfg *config,
	info *MessageInfo,
	msg *pubsub.Message,
) (context.Context, trace.Span) {
	if cfg == nil || !cfg.enableOTel {
		return ctx, nil
	}

	if cfg.spanStrategy == SpanStrategyAuto && !cfg.publicEndpoint {
		current := trace.SpanContextFromContext(ctx)
		if current.IsValid() && !current.IsRemote() {
			return ctx, nil
		}
	}

	attrs := consumerSpanAttrs(projectID, cfg, info, msg)
	spanOpts := []trace.SpanStartOption{
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(attrs...),
	}
	if cfg.publicEndpoint {
		spanOpts = append(spanOpts, trace.WithNewRoot())
		if extracted.IsValid() && extracted.IsRemote() {
			spanOpts = append(spanOpts, trace.WithLinks(trace.Link{SpanContext: extracted}))
		}
	}

	spanName := cfg.spanName
	if strings.TrimSpace(spanName) == "" {
		spanName = "pubsub.process"
	}
	return tracer.Start(ctx, spanName, spanOpts...)
}

func consumerSpanAttrs(projectID string, cfg *config, info *MessageInfo, msg *pubsub.Message) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, 10+len(cfg.spanAttributes))
	attrs = append(attrs, semconv.MessagingSystemGCPPubsub)
	attrs = append(attrs, semconv.MessagingOperationName("process"))
	attrs = append(attrs, semconv.MessagingOperationTypeDeliver)

	if info != nil {
		if subID := strings.TrimSpace(info.SubscriptionID); subID != "" {
			attrs = append(attrs, semconv.MessagingDestinationName(subID))
		}
		if topicID := strings.TrimSpace(info.TopicID); topicID != "" {
			attrs = append(attrs, semconv.MessagingDestinationPublishName(topicID))
		}
	}

	if msg != nil {
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
	}

	if projectID != "" {
		attrs = append(attrs, attribute.String("gcp.project_id", projectID))
	}

	if cfg != nil && len(cfg.spanAttributes) > 0 {
		attrs = append(attrs, cfg.spanAttributes...)
	}
	return attrs
}

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

func (mi *MessageInfo) loggerAttrs(cfg *config, msg *pubsub.Message, traceAttrs []slog.Attr) []slog.Attr {
	attrs := make([]slog.Attr, 0, len(traceAttrs)+10)
	if len(traceAttrs) > 0 {
		attrs = append(attrs, traceAttrs...)
	}

	if cfg != nil && cfg.publicEndpoint && mi != nil && mi.RemoteTraceparent != "" {
		attrs = append(attrs, slog.String("pubsub.remote.traceparent", mi.RemoteTraceparent))
	}

	attrs = append(attrs, slog.String(string(semconv.MessagingSystemGCPPubsub.Key), semconv.MessagingSystemGCPPubsub.Value.AsString()))
	attrs = append(attrs, slog.String(string(semconv.MessagingOperationNameKey), "process"))
	attrs = append(attrs, slog.String(string(semconv.MessagingOperationTypeDeliver.Key), semconv.MessagingOperationTypeDeliver.Value.AsString()))

	if mi != nil {
		if subID := strings.TrimSpace(mi.SubscriptionID); subID != "" {
			attrs = append(attrs, slog.String(string(semconv.MessagingDestinationNameKey), subID))
		}
		if topicID := strings.TrimSpace(mi.TopicID); topicID != "" {
			attrs = append(attrs, slog.String(string(semconv.MessagingDestinationPublishNameKey), topicID))
		}
	}

	if msg != nil {
		if (cfg == nil || cfg.logMessageID) && msg.ID != "" {
			attrs = append(attrs, slog.String(string(semconv.MessagingMessageIDKey), msg.ID))
		}
		if len(msg.Data) > 0 {
			bodySizeKV := semconv.MessagingMessageBodySize(len(msg.Data))
			attrs = append(attrs, slog.Int(string(bodySizeKV.Key), int(bodySizeKV.Value.AsInt64())))
		}
		if (cfg == nil || cfg.logOrderingKey) && msg.OrderingKey != "" {
			orderKV := semconv.MessagingGCPPubsubMessageOrderingKey(msg.OrderingKey)
			attrs = append(attrs, slog.String(string(orderKV.Key), orderKV.Value.AsString()))
		}
		if (cfg == nil || cfg.logDeliveryAttempt) && msg.DeliveryAttempt != nil {
			attemptKV := semconv.MessagingGCPPubsubMessageDeliveryAttempt(*msg.DeliveryAttempt)
			attrs = append(attrs, slog.Int(string(attemptKV.Key), int(attemptKV.Value.AsInt64())))
		}
		if (cfg == nil || cfg.logPublishTime) && !msg.PublishTime.IsZero() {
			attrs = append(attrs, slog.Time("pubsub.message.publish_time", msg.PublishTime))
		}
	}

	return attrs
}

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

func formatTraceparent(sc trace.SpanContext) string {
	if !sc.IsValid() {
		return ""
	}
	return fmt.Sprintf("00-%s-%s-%02x", sc.TraceID().String(), sc.SpanID().String(), byte(sc.TraceFlags()))
}
