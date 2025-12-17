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
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/pubsub"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/pjscruggs/slogcp"
)

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

func TestInjectDoesNotAllocateWithoutSpanContext(t *testing.T) {
	attrs := InjectAttributes(context.Background(), nil, WithPropagators(propagation.TraceContext{}))
	if attrs != nil {
		t.Fatalf("expected nil attrs when nothing is injected, got %#v", attrs)
	}
}

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
	if roSpan.Name() != "pubsub.process" {
		t.Fatalf("unexpected span name: %q", roSpan.Name())
	}
	if roSpan.Parent().SpanID() != spanCtx.SpanID() {
		t.Fatalf("expected parent span id %s, got %s", spanCtx.SpanID(), roSpan.Parent().SpanID())
	}
}

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
		if roSpan.Name() != "pubsub.process" {
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
		t.Fatalf("expected pubsub.process span to be created")
	}
}

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

func slogcpLoggerForTest(w *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}
