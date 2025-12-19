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
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"
	"unsafe"

	"cloud.google.com/go/pubsub/v2"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/pjscruggs/slogcp"
)

// mustSpanContext returns a deterministic, sampled SpanContext for tests.
func mustSpanContext(t *testing.T) trace.SpanContext {
	t.Helper()

	traceID, err := trace.TraceIDFromHex("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("parse trace id: %v", err)
	}
	spanID, err := trace.SpanIDFromHex("0123456789abcdef")
	if err != nil {
		t.Fatalf("parse span id: %v", err)
	}
	return trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	})
}

// decodeLogLine decodes a single JSON log line into a map for assertions.
func decodeLogLine(t *testing.T, line string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(line))
	dec.UseNumber()
	var payload map[string]any
	if err := dec.Decode(&payload); err != nil {
		t.Fatalf("decode log JSON: %v", err)
	}
	return payload
}

// setUnexportedStringField updates a struct string field via reflection, even if unexported.
func setUnexportedStringField(t *testing.T, target any, field, value string) {
	t.Helper()

	v := reflect.ValueOf(target)
	if v.Kind() != reflect.Ptr || v.IsNil() {
		t.Fatalf("target must be a non-nil pointer, got %T", target)
	}
	elem := v.Elem()
	if elem.Kind() != reflect.Struct {
		t.Fatalf("target must point to a struct, got %T", target)
	}
	f := elem.FieldByName(field)
	if !f.IsValid() {
		t.Fatalf("field %q not found on %T", field, target)
	}
	if f.Kind() != reflect.String {
		t.Fatalf("field %q must be a string, got %s", field, f.Kind())
	}
	if !f.CanAddr() {
		t.Fatalf("field %q is not addressable on %T", field, target)
	}
	reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem().SetString(value)
}

// TestInjectExtractRoundTrip verifies Inject and Extract preserve trace context.
func TestInjectExtractRoundTrip(t *testing.T) {
	spanCtx := mustSpanContext(t)
	ctx := trace.ContextWithSpanContext(context.Background(), spanCtx)

	msg := &pubsub.Message{}
	Inject(ctx, msg, WithPropagators(propagation.TraceContext{}))

	if msg.Attributes == nil {
		t.Fatalf("expected Inject to allocate attributes")
	}
	if msg.Attributes["traceparent"] == "" {
		t.Fatalf("expected Inject to set traceparent")
	}

	extracted, sc := Extract(context.Background(), msg, WithPropagators(propagation.TraceContext{}))
	if extracted == nil {
		t.Fatalf("expected non-nil context")
	}
	if !sc.IsValid() {
		t.Fatalf("expected extracted span context to be valid")
	}
	if sc.TraceID() != spanCtx.TraceID() {
		t.Fatalf("trace ID mismatch: got %s want %s", sc.TraceID(), spanCtx.TraceID())
	}
	if sc.SpanID() != spanCtx.SpanID() {
		t.Fatalf("span ID mismatch: got %s want %s", sc.SpanID(), spanCtx.SpanID())
	}
}

// TestInjectDoesNotAllocateWithoutSpanContext verifies injection is a no-op without trace context.
func TestInjectDoesNotAllocateWithoutSpanContext(t *testing.T) {
	attrs := InjectAttributes(context.Background(), nil, WithPropagators(propagation.TraceContext{}))
	if attrs != nil {
		t.Fatalf("expected nil attrs when nothing is injected, got %#v", attrs)
	}
}

// TestExtractCaseInsensitiveIsOptIn verifies strict extraction ignores case variants by default.
func TestExtractCaseInsensitiveIsOptIn(t *testing.T) {
	spanCtx := mustSpanContext(t)
	ctx := trace.ContextWithSpanContext(context.Background(), spanCtx)
	attrs := InjectAttributes(ctx, nil, WithPropagators(propagation.TraceContext{}))

	upper := map[string]string{
		"TraceParent": attrs["traceparent"],
		"TraceState":  attrs["tracestate"],
	}

	_, sc := ExtractAttributes(context.Background(), upper, WithPropagators(propagation.TraceContext{}))
	if sc.IsValid() {
		t.Fatalf("expected strict extraction to ignore case variants")
	}

	_, sc = ExtractAttributes(context.Background(), upper,
		WithPropagators(propagation.TraceContext{}),
		WithCaseInsensitiveExtraction(true),
	)
	if !sc.IsValid() {
		t.Fatalf("expected case-insensitive extraction to succeed")
	}
	if sc.TraceID() != spanCtx.TraceID() {
		t.Fatalf("trace ID mismatch: got %s want %s", sc.TraceID(), spanCtx.TraceID())
	}
}

// TestExtractGoogClientFallback verifies extraction can fall back to googclient_-prefixed keys.
func TestExtractGoogClientFallback(t *testing.T) {
	spanCtx := mustSpanContext(t)
	ctx := trace.ContextWithSpanContext(context.Background(), spanCtx)

	msg := &pubsub.Message{}
	Inject(ctx, msg,
		WithPropagators(propagation.TraceContext{}),
		WithGoogClientInjection(true),
	)

	attrs := map[string]string{
		"googclient_traceparent": msg.Attributes["googclient_traceparent"],
		"googclient_tracestate":  msg.Attributes["googclient_tracestate"],
	}

	_, sc := ExtractAttributes(context.Background(), attrs,
		WithPropagators(propagation.TraceContext{}),
		WithGoogClientExtraction(true),
	)
	if !sc.IsValid() {
		t.Fatalf("expected googclient extraction to succeed")
	}
	if sc.TraceID() != spanCtx.TraceID() {
		t.Fatalf("trace ID mismatch: got %s want %s", sc.TraceID(), spanCtx.TraceID())
	}
	if sc.SpanID() != spanCtx.SpanID() {
		t.Fatalf("span ID mismatch: got %s want %s", sc.SpanID(), spanCtx.SpanID())
	}
}

// TestWrapReceiveHandlerStartsSpanAndDerivesLogger verifies handler spans and log correlation are created.
func TestWrapReceiveHandlerStartsSpanAndDerivesLogger(t *testing.T) {
	spanCtx := mustSpanContext(t)
	upstreamCtx := trace.ContextWithSpanContext(context.Background(), spanCtx)
	attrs := InjectAttributes(upstreamCtx, nil, WithPropagators(propagation.TraceContext{}))

	msg := &pubsub.Message{
		ID:         "msg-123",
		Data:       []byte("hello"),
		Attributes: attrs,
	}

	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	logBuf := &bytes.Buffer{}
	baseLogger := slogcpLoggerForTest(logBuf)

	var sawInfo bool
	wrapped := WrapReceiveHandler(func(ctx context.Context, msg *pubsub.Message) {
		info, ok := InfoFromContext(ctx)
		if !ok || info == nil {
			t.Fatalf("expected MessageInfo in context")
		}
		if info.SubscriptionID != "sub-1" {
			t.Fatalf("unexpected subscription id: %q", info.SubscriptionID)
		}
		if info.MessageID != "msg-123" {
			t.Fatalf("unexpected message id: %q", info.MessageID)
		}
		slogcp.Logger(ctx).Info("handled")
		sawInfo = true
	}, WithLogger(baseLogger),
		WithProjectID("proj-123"),
		WithSubscriptionID("sub-1"),
		WithLogMessageID(true),
		WithTracerProvider(tp),
		WithPropagators(propagation.TraceContext{}),
	)

	wrapped(context.Background(), msg)

	if !sawInfo {
		t.Fatalf("expected handler to run")
	}

	lines := strings.Split(strings.TrimSpace(logBuf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 log line, got %d", len(lines))
	}
	payload := decodeLogLine(t, lines[0])

	if payload["messaging.system"] != "gcp_pubsub" {
		t.Fatalf("expected messaging.system gcp_pubsub, got %v", payload["messaging.system"])
	}
	if payload["messaging.destination.name"] != "sub-1" {
		t.Fatalf("expected messaging.destination.name sub-1, got %v", payload["messaging.destination.name"])
	}
	if payload["messaging.message.id"] != "msg-123" {
		t.Fatalf("expected messaging.message.id msg-123, got %v", payload["messaging.message.id"])
	}

	traceField, ok := payload[slogcp.TraceKey].(string)
	if !ok || !strings.Contains(traceField, spanCtx.TraceID().String()) {
		t.Fatalf("expected trace field to contain %s, got %v", spanCtx.TraceID().String(), payload[slogcp.TraceKey])
	}
	if payload[slogcp.SpanKey] == nil {
		t.Fatalf("expected span id field to be present")
	}

	ended := recorder.Ended()
	if len(ended) != 1 {
		t.Fatalf("expected 1 ended span, got %d", len(ended))
	}
	roSpan := ended[0]
	if roSpan.Name() != defaultConsumerSpanName {
		t.Fatalf("unexpected span name: %q", roSpan.Name())
	}
	if roSpan.Parent().SpanID() != spanCtx.SpanID() {
		t.Fatalf("expected parent span id %s, got %s", spanCtx.SpanID(), roSpan.Parent().SpanID())
	}
}

// TestWrapReceiveHandlerParentsToRemoteWhenSpanAlreadyActive ensures extraction overrides a local parent span.
func TestWrapReceiveHandlerParentsToRemoteWhenSpanAlreadyActive(t *testing.T) {
	remoteSpanCtx := mustSpanContext(t)
	remoteCtx := trace.ContextWithSpanContext(context.Background(), remoteSpanCtx)
	attrs := InjectAttributes(remoteCtx, nil, WithPropagators(propagation.TraceContext{}))

	msg := &pubsub.Message{
		ID:         "msg-123",
		Attributes: attrs,
	}

	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	ctxWithLocal, localSpan := tp.Tracer("test").Start(context.Background(), "local")

	wrapped := WrapReceiveHandler(func(ctx context.Context, msg *pubsub.Message) {},
		WithTracerProvider(tp),
		WithPropagators(propagation.TraceContext{}),
	)

	wrapped(ctxWithLocal, msg)
	localSpan.End()

	ended := recorder.Ended()
	var sawProcess bool
	for _, roSpan := range ended {
		if roSpan.Name() != defaultConsumerSpanName {
			continue
		}
		sawProcess = true
		if roSpan.SpanContext().TraceID() != remoteSpanCtx.TraceID() {
			t.Fatalf("expected handler span to use trace ID %s, got %s", remoteSpanCtx.TraceID(), roSpan.SpanContext().TraceID())
		}
		if roSpan.Parent().SpanID() != remoteSpanCtx.SpanID() {
			t.Fatalf("expected handler span parent ID %s, got %s", remoteSpanCtx.SpanID(), roSpan.Parent().SpanID())
		}
	}
	if !sawProcess {
		t.Fatalf("expected %s span to be created", defaultConsumerSpanName)
	}
}

// TestWrapReceiveHandlerOmitsMessageIDByDefault verifies high-cardinality log fields are disabled by default.
func TestWrapReceiveHandlerOmitsMessageIDByDefault(t *testing.T) {
	msg := &pubsub.Message{
		ID:   "msg-123",
		Data: []byte("hello"),
	}

	logBuf := &bytes.Buffer{}
	baseLogger := slogcpLoggerForTest(logBuf)

	wrapped := WrapReceiveHandler(func(ctx context.Context, msg *pubsub.Message) {
		slogcp.Logger(ctx).Info("handled")
	}, WithLogger(baseLogger),
		WithSubscriptionID("sub-1"),
		WithOTel(false),
	)

	wrapped(context.Background(), msg)

	lines := strings.Split(strings.TrimSpace(logBuf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 log line, got %d", len(lines))
	}
	payload := decodeLogLine(t, lines[0])

	if _, ok := payload["messaging.message.id"]; ok {
		t.Fatalf("expected messaging.message.id to be absent by default, got %v", payload["messaging.message.id"])
	}
}

// TestWrapReceiveHandlerPublicEndpointLinksRemote verifies public endpoint mode starts a new root trace and links remote context.
func TestWrapReceiveHandlerPublicEndpointLinksRemote(t *testing.T) {
	spanCtx := mustSpanContext(t)
	upstreamCtx := trace.ContextWithSpanContext(context.Background(), spanCtx)
	attrs := InjectAttributes(upstreamCtx, nil, WithPropagators(propagation.TraceContext{}))

	msg := &pubsub.Message{
		ID:          "msg-123",
		Data:        []byte("hello"),
		Attributes:  attrs,
		PublishTime: time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC),
	}

	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	logBuf := &bytes.Buffer{}
	baseLogger := slogcpLoggerForTest(logBuf)

	wrapped := WrapReceiveHandler(func(ctx context.Context, msg *pubsub.Message) {
		slogcp.Logger(ctx).Info("handled")
	}, WithLogger(baseLogger),
		WithProjectID("proj-123"),
		WithSubscriptionID("sub-1"),
		WithTracerProvider(tp),
		WithPropagators(propagation.TraceContext{}),
		WithPublicEndpoint(true),
	)

	wrapped(context.Background(), msg)

	ended := recorder.Ended()
	if len(ended) != 1 {
		t.Fatalf("expected 1 ended span, got %d", len(ended))
	}
	roSpan := ended[0]
	if roSpan.Parent().IsValid() {
		t.Fatalf("expected root span in public endpoint mode")
	}
	links := roSpan.Links()
	if len(links) != 1 {
		t.Fatalf("expected 1 link, got %d", len(links))
	}
	if links[0].SpanContext.TraceID() != spanCtx.TraceID() {
		t.Fatalf("expected link to trace %s, got %s", spanCtx.TraceID(), links[0].SpanContext.TraceID())
	}

	lines := strings.Split(strings.TrimSpace(logBuf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 log line, got %d", len(lines))
	}
	payload := decodeLogLine(t, lines[0])
	traceField, ok := payload[slogcp.TraceKey].(string)
	if !ok || strings.Contains(traceField, spanCtx.TraceID().String()) {
		t.Fatalf("expected trace field to use new trace id, got %v", payload[slogcp.TraceKey])
	}
}

// TestPublicEndpointWithoutSpanDoesNotCorrelateLogs verifies untrusted remote context is not used for log correlation by default.
func TestPublicEndpointWithoutSpanDoesNotCorrelateLogs(t *testing.T) {
	spanCtx := mustSpanContext(t)
	upstreamCtx := trace.ContextWithSpanContext(context.Background(), spanCtx)
	attrs := InjectAttributes(upstreamCtx, nil, WithPropagators(propagation.TraceContext{}))

	msg := &pubsub.Message{
		ID:         "msg-123",
		Data:       []byte("hello"),
		Attributes: attrs,
	}

	logBuf := &bytes.Buffer{}
	baseLogger := slogcpLoggerForTest(logBuf)

	wrapped := WrapReceiveHandler(func(ctx context.Context, msg *pubsub.Message) {
		slogcp.Logger(ctx).Info("handled")
	}, WithLogger(baseLogger),
		WithProjectID("proj-123"),
		WithSubscriptionID("sub-1"),
		WithPropagators(propagation.TraceContext{}),
		WithPublicEndpoint(true),
		WithOTel(false),
	)

	wrapped(context.Background(), msg)

	lines := strings.Split(strings.TrimSpace(logBuf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 log line, got %d", len(lines))
	}
	payload := decodeLogLine(t, lines[0])
	if _, ok := payload[slogcp.TraceKey]; ok {
		t.Fatalf("expected no trace correlation field, got %v", payload[slogcp.TraceKey])
	}
}

// TestPublicEndpointWithoutSpanCanCorrelateLogsWhenEnabled verifies optional log correlation to remote context.
func TestPublicEndpointWithoutSpanCanCorrelateLogsWhenEnabled(t *testing.T) {
	spanCtx := mustSpanContext(t)
	upstreamCtx := trace.ContextWithSpanContext(context.Background(), spanCtx)
	attrs := InjectAttributes(upstreamCtx, nil, WithPropagators(propagation.TraceContext{}))

	msg := &pubsub.Message{
		ID:         "msg-123",
		Data:       []byte("hello"),
		Attributes: attrs,
	}

	logBuf := &bytes.Buffer{}
	baseLogger := slogcpLoggerForTest(logBuf)

	wrapped := WrapReceiveHandler(func(ctx context.Context, msg *pubsub.Message) {
		slogcp.Logger(ctx).Info("handled")
	}, WithLogger(baseLogger),
		WithProjectID("proj-123"),
		WithSubscriptionID("sub-1"),
		WithPropagators(propagation.TraceContext{}),
		WithPublicEndpoint(true),
		WithOTel(false),
		WithPublicEndpointCorrelateLogsToRemote(true),
	)

	wrapped(context.Background(), msg)

	lines := strings.Split(strings.TrimSpace(logBuf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 log line, got %d", len(lines))
	}
	payload := decodeLogLine(t, lines[0])
	traceField, ok := payload[slogcp.TraceKey].(string)
	if !ok || !strings.Contains(traceField, spanCtx.TraceID().String()) {
		t.Fatalf("expected trace field to contain %s, got %v", spanCtx.TraceID().String(), payload[slogcp.TraceKey])
	}
}

// TestApplyOptionsCoversSetters exercises option helpers for coverage.
func TestApplyOptionsCoversSetters(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	baseLogger := slogcpLoggerForTest(&bytes.Buffer{})

	sub := &pubsub.Subscriber{}
	setUnexportedStringField(t, sub, "name", "projects/p/subscriptions/sub-1")

	topic := &pubsub.Publisher{}
	setUnexportedStringField(t, topic, "name", "projects/p/topics/topic-1")

	cfg := applyOptions([]Option{
		nil,
		WithLogger(nil),
		WithLogger(baseLogger),
		WithProjectID("proj-123"),
		WithSubscription(nil),
		WithSubscription(sub),
		WithSubscriptionID(" sub-override "),
		WithTopic(nil),
		WithTopic(topic),
		WithTopicID(" topic-override "),
		WithOTel(false),
		WithSpanStrategy(SpanStrategyAuto),
		WithTracerProvider(tp),
		WithPublicEndpoint(true),
		WithPublicEndpointCorrelateLogsToRemote(true),
		WithPropagators(nil),
		WithTracePropagation(false),
		WithBaggagePropagation(true),
		WithCaseInsensitiveExtraction(true),
		WithInjectOnlyIfSpanPresent(true),
		WithSpanName(" "),
		WithSpanName(" custom-span "),
		WithSpanAttributes(attribute.String("k", "v")),
		WithLogMessageID(true),
		WithLogOrderingKey(true),
		WithLogDeliveryAttempt(false),
		WithLogPublishTime(true),
		WithAttrEnricher(nil),
		WithAttrEnricher(func(context.Context, *pubsub.Message, *MessageInfo) []slog.Attr {
			return []slog.Attr{slog.String("extra", "value")}
		}),
		WithAttrTransformer(nil),
		WithAttrTransformer(func(ctx context.Context, attrs []slog.Attr, msg *pubsub.Message, info *MessageInfo) []slog.Attr {
			return attrs
		}),
		WithGoogClientCompat(true),
	})

	if cfg.logger != baseLogger {
		t.Fatalf("logger not applied")
	}
	if cfg.projectID != "proj-123" {
		t.Fatalf("projectID = %q", cfg.projectID)
	}
	if cfg.subscriptionID != "sub-override" {
		t.Fatalf("subscriptionID = %q", cfg.subscriptionID)
	}
	if cfg.topicID != "topic-override" {
		t.Fatalf("topicID = %q", cfg.topicID)
	}
	if cfg.enableOTel {
		t.Fatalf("enableOTel should be false")
	}
	if cfg.spanStrategy != SpanStrategyAuto {
		t.Fatalf("spanStrategy = %v", cfg.spanStrategy)
	}
	if cfg.tracerProvider != tp {
		t.Fatalf("tracerProvider not applied")
	}
	if !cfg.publicEndpoint {
		t.Fatalf("publicEndpoint should be true")
	}
	if !cfg.publicEndpointCorrelateLogsToRemote {
		t.Fatalf("publicEndpointCorrelateLogsToRemote should be true")
	}
	if cfg.propagatorsSet || cfg.propagators != nil {
		t.Fatalf("expected propagators to be unset and nil")
	}
	if cfg.propagateTrace {
		t.Fatalf("propagateTrace should be false")
	}
	if !cfg.propagateBaggage {
		t.Fatalf("propagateBaggage should be true")
	}
	if !cfg.caseInsensitiveExtraction {
		t.Fatalf("caseInsensitiveExtraction should be true")
	}
	if !cfg.injectOnlyIfSpanPresent {
		t.Fatalf("injectOnlyIfSpanPresent should be true")
	}
	if cfg.spanName != "custom-span" {
		t.Fatalf("spanName = %q", cfg.spanName)
	}
	if len(cfg.spanAttributes) != 1 {
		t.Fatalf("spanAttributes length = %d", len(cfg.spanAttributes))
	}
	if !cfg.logMessageID {
		t.Fatalf("logMessageID should be true")
	}
	if !cfg.logOrderingKey {
		t.Fatalf("logOrderingKey should be true")
	}
	if cfg.logDeliveryAttempt {
		t.Fatalf("logDeliveryAttempt should be false")
	}
	if !cfg.logPublishTime {
		t.Fatalf("logPublishTime should be true")
	}
	if len(cfg.attrEnrichers) != 1 {
		t.Fatalf("attrEnrichers length = %d", len(cfg.attrEnrichers))
	}
	if len(cfg.attrTransformers) != 1 {
		t.Fatalf("attrTransformers length = %d", len(cfg.attrTransformers))
	}
	if !cfg.googClientExtraction || !cfg.googClientInjection {
		t.Fatalf("googclient compat flags not set")
	}
}

// TestPropagationCoverBranches exercises propagation edge cases for coverage.
func TestPropagationCoverBranches(t *testing.T) {
	spanCtx := mustSpanContext(t)
	ctxWithSpan := trace.ContextWithSpanContext(context.Background(), spanCtx)
	origPropagator := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTextMapPropagator(origPropagator)
	})

	Inject(ctxWithSpan, nil)
	ctx, sc := Extract(ctxWithSpan, nil)
	if ctx != ctxWithSpan {
		t.Fatalf("expected Extract to return original context when msg is nil")
	}
	if !sc.IsValid() {
		t.Fatalf("expected Extract to return the current span context")
	}

	input := map[string]string{"k": "v"}
	var nilCtx context.Context
	if out := injectAttributes(context.Background(), input, &config{propagateTrace: false}); out["k"] != "v" || len(out) != 1 {
		t.Fatalf("expected disabled propagation to preserve input, got %#v", out)
	}
	if out := injectAttributes(nilCtx, input, &config{propagateTrace: true}); out["k"] != "v" || len(out) != 1 {
		t.Fatalf("expected nil ctx to preserve input, got %#v", out)
	}

	if out := InjectAttributes(context.Background(), nil,
		WithPropagators(propagation.TraceContext{}),
		WithInjectOnlyIfSpanPresent(true),
	); out != nil {
		t.Fatalf("expected inject-only-if-span to be a no-op without a span")
	}

	out := InjectAttributes(ctxWithSpan, nil, WithPropagators(nil))
	if out == nil || out["traceparent"] == "" {
		t.Fatalf("expected nil propagator to use global traceparent injection")
	}

	out = InjectAttributes(ctxWithSpan, nil, WithPropagators(nil), WithGoogClientInjection(true))
	if out == nil || out["googclient_traceparent"] == "" {
		t.Fatalf("expected googclient injection to write traceparent")
	}

	_, sc = ExtractAttributes(nilCtx, nil, WithPropagators(propagation.TraceContext{}))
	if sc.IsValid() {
		t.Fatalf("expected empty extraction with nil attrs")
	}

	globalAttrs := InjectAttributes(ctxWithSpan, nil)
	if globalAttrs == nil || globalAttrs["traceparent"] == "" {
		t.Fatalf("expected global propagator injection to set traceparent")
	}
	if _, sc := ExtractAttributes(context.Background(), globalAttrs); !sc.IsValid() {
		t.Fatalf("expected global propagator extraction to succeed")
	}
	if cfgNil := injectAttributes(ctxWithSpan, nil, nil); cfgNil == nil || cfgNil["traceparent"] == "" {
		t.Fatalf("expected nil cfg injection to use defaults")
	}

	cfgNilCtx, sc := ensureSpanContext(context.Background(), globalAttrs, nil)
	if cfgNilCtx == nil || !sc.IsValid() {
		t.Fatalf("expected ensureSpanContext with nil cfg to extract span context")
	}
	if _, sc := ExtractAttributes(context.Background(), globalAttrs, WithTracePropagation(false)); sc.IsValid() {
		t.Fatalf("expected ExtractAttributes to skip extraction when propagation is disabled")
	}
	cfgNoProp := defaultConfig()
	cfgNoProp.propagatorsSet = true
	cfgNoProp.propagators = nil
	if _, sc := extractSpanContext(globalAttrs, cfgNoProp); sc.IsValid() {
		t.Fatalf("expected extractSpanContext to skip extraction when propagator is explicitly nil")
	}

	missingAttrs := map[string]string{"k": "v"}
	cfgGoog := applyOptions([]Option{WithPropagators(propagation.TraceContext{}), WithGoogClientExtraction(true)})
	if _, sc := ensureSpanContext(context.Background(), missingAttrs, cfgGoog); sc.IsValid() {
		t.Fatalf("expected ensureSpanContext to return invalid span context when no trace keys exist")
	}

	if _, sc := ExtractAttributes(context.Background(), globalAttrs, WithPropagators(nil)); !sc.IsValid() {
		t.Fatalf("expected ExtractAttributes to use global propagator when nil is supplied")
	}

	outWithUpper := map[string]string{
		"GOOGCLIENT_TRACEPARENT": out["googclient_traceparent"],
		"GOOGCLIENT_TRACESTATE":  out["googclient_tracestate"],
	}
	_, sc = ExtractAttributes(context.Background(), outWithUpper,
		WithPropagators(propagation.TraceContext{}),
		WithGoogClientExtraction(true),
		WithCaseInsensitiveExtraction(true),
	)
	if !sc.IsValid() {
		t.Fatalf("expected case-insensitive googclient extraction to succeed")
	}

	spanMember, err := baggage.NewMember("k", "v")
	if err != nil {
		t.Fatalf("new baggage member: %v", err)
	}
	bag, err := baggage.New(spanMember)
	if err != nil {
		t.Fatalf("new baggage: %v", err)
	}
	ctxWithBag := baggage.ContextWithBaggage(ctxWithSpan, bag)
	composite := propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{})
	attrs := InjectAttributes(ctxWithBag, nil,
		WithPropagators(composite),
		WithBaggagePropagation(true),
	)
	extractedCtx, sc := ExtractAttributes(context.Background(), attrs,
		WithPropagators(composite),
		WithBaggagePropagation(true),
	)
	if !sc.IsValid() {
		t.Fatalf("expected extracted span context to be valid")
	}
	if got := baggage.FromContext(extractedCtx).Member("k").Value(); got != "v" {
		t.Fatalf("expected extracted baggage value v, got %q", got)
	}

	extracted := trace.ContextWithSpanContext(context.Background(), spanCtx)
	applied := applyExtractedContext(context.Background(), extracted, nil)
	if !trace.SpanContextFromContext(applied).IsValid() {
		t.Fatalf("expected applyExtractedContext to preserve span context")
	}
}

// TestCarrierImplementationsCoverBranches directly exercises carrier implementations for coverage.
func TestCarrierImplementationsCoverBranches(t *testing.T) {
	_ = (lazyCarrier{}).Get("traceparent")
	(lazyCarrier{}).Set("traceparent", "ignored")
	if keys := (lazyCarrier{}).Keys(); keys != nil {
		t.Fatalf("expected nil Keys for nil carrier, got %v", keys)
	}

	var attrs map[string]string
	c := lazyCarrier{attrs: &attrs, allowBaggage: false}
	if got := c.Get("traceparent"); got != "" {
		t.Fatalf("expected empty Get with nil map, got %q", got)
	}
	if got := c.Get("baggage"); got != "" {
		t.Fatalf("expected baggage Get to be ignored, got %q", got)
	}
	c.Set("baggage", "k=v")
	if attrs != nil {
		t.Fatalf("expected baggage to be ignored when disabled")
	}
	c.Set("TraceParent", "abc")
	if attrs["traceparent"] != "abc" {
		t.Fatalf("expected traceparent to be set")
	}
	if got := c.Get("TraceParent"); got != "abc" {
		t.Fatalf("expected Get to return abc, got %q", got)
	}
	if got := c.Get("baggage"); got != "" {
		t.Fatalf("expected baggage Get to be ignored, got %q", got)
	}

	withPrefix := lazyCarrier{attrs: &attrs, prefix: googclientPrefix, allowBaggage: true}
	withPrefix.Set("traceparent", "def")
	if attrs["googclient_traceparent"] != "def" {
		t.Fatalf("expected prefixed traceparent to be set")
	}
	if got := withPrefix.Get("traceparent"); got != "def" {
		t.Fatalf("expected prefixed Get to return def, got %q", got)
	}
	if keys := withPrefix.Keys(); len(keys) < 2 {
		t.Fatalf("expected Keys to include entries, got %v", keys)
	}

	empty := map[string]string{}
	emptyCarrier := lazyCarrier{attrs: &empty}
	if keys := emptyCarrier.Keys(); keys != nil {
		t.Fatalf("expected nil Keys for empty map, got %v", keys)
	}

	emptyCI := &caseInsensitiveCarrier{attrs: map[string]string{}}
	if got := emptyCI.Get("traceparent"); got != "" {
		t.Fatalf("expected empty case-insensitive Get with empty map, got %q", got)
	}
	if keys := emptyCI.Keys(); keys != nil {
		t.Fatalf("expected Keys to return nil for empty map, got %v", keys)
	}

	ci := &caseInsensitiveCarrier{attrs: map[string]string{"TraceParent": "abc"}, allowBaggage: false}
	if got := ci.Get("traceparent"); got != "abc" {
		t.Fatalf("expected case-insensitive lookup to find TraceParent, got %q", got)
	}
	if got := ci.Get("baggage"); got != "" {
		t.Fatalf("expected case-insensitive Get to ignore baggage, got %q", got)
	}
	ci.Set("TraceState", "state")
	if ci.attrs["tracestate"] != "state" {
		t.Fatalf("expected tracestate to be set")
	}
	ci.Set("baggage", "k=v")
	if _, ok := ci.attrs["baggage"]; ok {
		t.Fatalf("expected baggage to be ignored when disabled")
	}
	if keys := ci.Keys(); len(keys) == 0 {
		t.Fatalf("expected Keys to return entries")
	}

	ciPrefix := &caseInsensitiveCarrier{attrs: map[string]string{}, prefix: googclientPrefix, allowBaggage: true}
	ciPrefix.Set("TraceParent", "abc")
	if ciPrefix.attrs["googclient_traceparent"] != "abc" {
		t.Fatalf("expected prefixed traceparent to be set")
	}
	if got := ciPrefix.Get("traceparent"); got != "abc" {
		t.Fatalf("expected prefixed Get to return abc, got %q", got)
	}
	ciPrefixUpper := &caseInsensitiveCarrier{attrs: map[string]string{"GOOGCLIENT_TRACEPARENT": "xyz"}, prefix: googclientPrefix, allowBaggage: true}
	if got := ciPrefixUpper.Get("traceparent"); got != "xyz" {
		t.Fatalf("expected prefixed lookupLower to return xyz, got %q", got)
	}
	ciExact := &caseInsensitiveCarrier{attrs: map[string]string{"traceparent": "exact"}, allowBaggage: true}
	if got := ciExact.Get("traceparent"); got != "exact" {
		t.Fatalf("expected exact match to return exact, got %q", got)
	}
	if got := ciExact.Get("TraceParent"); got != "exact" {
		t.Fatalf("expected lowercase match to return exact, got %q", got)
	}
	(&caseInsensitiveCarrier{}).Set("traceparent", "ignored")

	strict := strictCarrier{attrs: map[string]string{"traceparent": "abc"}, allowBaggage: false}
	if got := strict.Get("traceparent"); got != "abc" {
		t.Fatalf("expected strict Get to return abc, got %q", got)
	}
	if got := strict.Get("baggage"); got != "" {
		t.Fatalf("expected strict Get to ignore baggage, got %q", got)
	}
	strict.Set("baggage", "k=v")
	if _, ok := strict.attrs["baggage"]; ok {
		t.Fatalf("expected strict Set to ignore baggage when disabled")
	}
	strictPrefix := strictCarrier{attrs: map[string]string{}, prefix: googclientPrefix, allowBaggage: true}
	strictPrefix.Set("traceparent", "abc")
	if strictPrefix.attrs["googclient_traceparent"] != "abc" {
		t.Fatalf("expected strict prefix to set traceparent")
	}
	if got := strictPrefix.Get("traceparent"); got != "abc" {
		t.Fatalf("expected strict prefixed Get to return abc, got %q", got)
	}
	if keys := strictPrefix.Keys(); len(keys) != 1 {
		t.Fatalf("expected strict prefix Keys length 1, got %d", len(keys))
	}

	strictEmpty := strictCarrier{attrs: map[string]string{}}
	if got := strictEmpty.Get("traceparent"); got != "" {
		t.Fatalf("expected strict Get to return empty for empty map, got %q", got)
	}
	if keys := strictEmpty.Keys(); keys != nil {
		t.Fatalf("expected strict Keys to return nil for empty map, got %v", keys)
	}
	(strictCarrier{}).Set("traceparent", "ignored")
}

// TestReceiveHelpersCoverBranches exercises receive helper branches for coverage.
func TestReceiveHelpersCoverBranches(t *testing.T) {
	var nilCtx context.Context
	if info, ok := InfoFromContext(nilCtx); ok || info != nil {
		t.Fatalf("expected nil context to return no info")
	}
	if info, ok := InfoFromContext(context.Background()); ok || info != nil {
		t.Fatalf("expected missing value to return no info")
	}
	if info, ok := InfoFromContext(context.WithValue(context.Background(), messageInfoKey{}, "bad")); ok || info != nil {
		t.Fatalf("expected wrong type to return no info")
	}
	if info, ok := InfoFromContext(context.WithValue(context.Background(), messageInfoKey{}, (*MessageInfo)(nil))); ok || info != nil {
		t.Fatalf("expected nil MessageInfo to return no info")
	}

	var nilInfo *MessageInfo
	if attempt, ok := nilInfo.DeliveryAttempt(); ok || attempt != 0 {
		t.Fatalf("expected nil DeliveryAttempt to return false")
	}

	msg := &pubsub.Message{Attributes: map[string]string{"k": "v"}}
	if got := msgAttributes(nil); got != nil {
		t.Fatalf("expected nil msgAttributes for nil message")
	}
	if got := msgAttributes(msg); got["k"] != "v" {
		t.Fatalf("expected msgAttributes to return underlying map")
	}

	if got := resolveProjectID(" proj "); got != "proj" {
		t.Fatalf("resolveProjectID explicit = %q", got)
	}
	if got := resolveProjectID(" "); got != strings.TrimSpace(got) {
		t.Fatalf("resolveProjectID runtime should be trimmed, got %q", got)
	}

	cfg := applyOptions([]Option{WithSubscriptionID("sub-1"), WithTopicID("topic-1")})
	remote := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    mustSpanContext(t).TraceID(),
		SpanID:     mustSpanContext(t).SpanID(),
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	})
	attempt := 2
	fullMsg := &pubsub.Message{
		ID:              "msg-123",
		Data:            []byte("hello"),
		OrderingKey:     "order",
		PublishTime:     time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC),
		DeliveryAttempt: &attempt,
	}
	info := newMessageInfo(cfg, fullMsg, remote)
	if info.RemoteTraceparent == "" {
		t.Fatalf("expected remote traceparent to be set")
	}
	if da, ok := info.DeliveryAttempt(); !ok || da != attempt {
		t.Fatalf("expected delivery attempt %d, got %d (ok=%v)", attempt, da, ok)
	}

	if got := resolveSpanName(" "); got != defaultConsumerSpanName {
		t.Fatalf("resolveSpanName blank = %q", got)
	}
	if got := resolveSpanName(" custom "); got != "custom" {
		t.Fatalf("resolveSpanName trimmed = %q", got)
	}

	cfg.publicEndpoint = true
	if got := consumerSpanOptions("", cfg, trace.SpanContext{}, info, fullMsg); len(got) != 3 {
		t.Fatalf("expected 3 span options without link, got %d", len(got))
	}
	if got := consumerSpanOptions("proj", cfg, remote, info, fullMsg); len(got) != 4 {
		t.Fatalf("expected 4 span options with link, got %d", len(got))
	}
	cfg.publicEndpoint = false
	if got := consumerSpanOptions("", cfg, remote, info, fullMsg); len(got) != 2 {
		t.Fatalf("expected 2 span options for trusted parent, got %d", len(got))
	}

	cfg.spanAttributes = []attribute.KeyValue{attribute.String("custom.attr", "v")}
	spanAttrs := consumerSpanAttrs("proj", cfg, info, fullMsg)
	var sawCustom bool
	for _, kv := range spanAttrs {
		if kv.Key == attribute.Key("custom.attr") {
			sawCustom = true
			break
		}
	}
	if !sawCustom {
		t.Fatalf("expected custom span attribute")
	}

	_ = consumerSpanAttrs("proj", nil, info, fullMsg)
	_ = appendConsumerDestinationAttrs([]attribute.KeyValue{}, nil)
	_ = appendConsumerMessageAttrs([]attribute.KeyValue{}, nil)
	_ = appendMessagingDestinationLoggerAttrs([]slog.Attr{}, nil)

	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	tracer := tp.Tracer("test")

	_, end := startConsumerSpan(context.Background(), trace.SpanContext{}, tracer, "", nil, info, nil)
	end()

	cfgDisabled := defaultConfig()
	cfgDisabled.enableOTel = false
	_, end = startConsumerSpan(context.Background(), trace.SpanContext{}, tracer, "", cfgDisabled, info, nil)
	end()

	cfgAuto := defaultConfig()
	cfgAuto.spanStrategy = SpanStrategyAuto
	ctxWithSpan, span := tracer.Start(context.Background(), "local")
	_, end = startConsumerSpan(ctxWithSpan, trace.SpanContext{}, tracer, "", cfgAuto, info, nil)
	end()
	span.End()

	_ = applyEnrichers(context.Background(), nil, []slog.Attr{}, fullMsg, info)
	_ = applyTransformers(context.Background(), nil, []slog.Attr{}, fullMsg, info)

	attrs := []slog.Attr{slog.String("k", "v")}
	noopEnricher := AttrEnricher(func(context.Context, *pubsub.Message, *MessageInfo) []slog.Attr { return nil })
	extraEnricher := AttrEnricher(func(context.Context, *pubsub.Message, *MessageInfo) []slog.Attr {
		return []slog.Attr{slog.String("extra", "value")}
	})
	cfg.attrEnrichers = []AttrEnricher{nil, noopEnricher, extraEnricher}

	dropAllTransformer := AttrTransformer(func(context.Context, []slog.Attr, *pubsub.Message, *MessageInfo) []slog.Attr { return []slog.Attr{} })
	cfg.attrTransformers = []AttrTransformer{nil, dropAllTransformer}
	attrs = applyEnrichers(context.Background(), cfg, attrs, fullMsg, info)
	attrs = applyTransformers(context.Background(), cfg, attrs, fullMsg, info)
	if len(attrs) != 0 {
		t.Fatalf("expected transformer to return empty attrs")
	}

	logAttrs := info.loggerAttrs(nil, fullMsg, []slog.Attr{slog.String("trace", "value")})
	if len(logAttrs) == 0 {
		t.Fatalf("expected loggerAttrs to return attributes")
	}

	_ = appendRemoteTraceparent([]slog.Attr{}, &config{publicEndpoint: true}, &MessageInfo{})
	_ = appendMessagingDestinationLoggerAttrs([]slog.Attr{}, &MessageInfo{SubscriptionID: "sub", TopicID: "topic"})
	_ = appendMessagingMessageLoggerAttrs([]slog.Attr{}, defaultConfig(), nil)

	cfg = defaultConfig()
	cfg.logMessageID = true
	cfg.logOrderingKey = true
	cfg.logDeliveryAttempt = true
	cfg.logPublishTime = true
	attrs = []slog.Attr{}
	attrs = appendMessageIDLoggerAttr(attrs, cfg, fullMsg)
	attrs = appendMessageBodySizeLoggerAttr(attrs, &pubsub.Message{})
	attrs = appendMessageBodySizeLoggerAttr(attrs, fullMsg)
	attrs = appendOrderingKeyLoggerAttr(attrs, cfg, &pubsub.Message{})
	attrs = appendOrderingKeyLoggerAttr(attrs, cfg, fullMsg)
	attrs = appendDeliveryAttemptLoggerAttr(attrs, &config{logDeliveryAttempt: false}, fullMsg)
	attrs = appendDeliveryAttemptLoggerAttr(attrs, cfg, &pubsub.Message{})
	attrs = appendDeliveryAttemptLoggerAttr(attrs, cfg, fullMsg)
	attrs = appendPublishTimeLoggerAttr(attrs, &config{logPublishTime: false}, fullMsg)
	attrs = appendPublishTimeLoggerAttr(attrs, cfg, &pubsub.Message{})
	attrs = appendPublishTimeLoggerAttr(attrs, cfg, fullMsg)
	if len(attrs) == 0 {
		t.Fatalf("expected attribute slice to be non-empty")
	}

	logger := loggerWithAttrs(nil, nil)
	_ = loggerWithAttrs(logger, nil)
	_ = loggerWithAttrs(nil, []slog.Attr{slog.String("k", "v")})

	if got := formatTraceparent(trace.SpanContext{}); got != "" {
		t.Fatalf("expected invalid traceparent to be empty, got %q", got)
	}
	if got := formatTraceparent(remote); got == "" {
		t.Fatalf("expected valid traceparent to be populated")
	}
}

// TestWrapReceiveHandlerNilInputs exercises nil handler and nil logger fallback branches.
func TestWrapReceiveHandlerNilInputs(t *testing.T) {
	wrapped := WrapReceiveHandler(nil)
	wrapped(context.Background(), nil)

	var sawLogger bool
	wrapped = WrapReceiveHandler(func(ctx context.Context, msg *pubsub.Message) {
		if slogcp.Logger(ctx) == nil {
			t.Fatalf("expected logger to be available")
		}
		sawLogger = true
	}, Option(func(cfg *config) { cfg.logger = nil }), WithOTel(false))

	wrapped(context.Background(), nil)
	if !sawLogger {
		t.Fatalf("expected handler to be invoked")
	}
}

// slogcpLoggerForTest creates a JSON logger for assertions.
func slogcpLoggerForTest(w *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}
