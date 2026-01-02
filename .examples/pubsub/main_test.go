// Copyright 2025-2026 Patrick J. Scruggs
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

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/pjscruggs/slogcp"
	"github.com/pjscruggs/slogcp/slogcppubsub"
)

// TestInjectAddsTraceparent verifies trace context is injected into attributes.
func TestInjectAddsTraceparent(t *testing.T) {
	t.Parallel()

	spanCtx := mustSpanContext(t)
	ctx := trace.ContextWithSpanContext(context.Background(), spanCtx)

	msg := &pubsub.Message{}
	slogcppubsub.Inject(ctx, msg, slogcppubsub.WithPropagators(propagation.TraceContext{}))

	if msg.Attributes == nil {
		t.Fatalf("expected attributes to be set")
	}
	traceparent := msg.Attributes["traceparent"]
	if traceparent == "" {
		t.Fatalf("expected traceparent to be injected")
	}
	if !strings.Contains(traceparent, spanCtx.TraceID().String()) {
		t.Fatalf("traceparent missing trace id: %q", traceparent)
	}
}

// TestWrapReceiveHandlerLogsMessageFields validates message-scoped log enrichment.
func TestWrapReceiveHandlerLogsMessageFields(t *testing.T) {
	t.Parallel()

	spanCtx := mustSpanContext(t)
	ctx := trace.ContextWithSpanContext(context.Background(), spanCtx)

	attempt := 2
	publishTime := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	msg := &pubsub.Message{
		ID:              "msg-123",
		Data:            []byte("hello"),
		OrderingKey:     "order-1",
		PublishTime:     publishTime,
		DeliveryAttempt: &attempt,
	}
	slogcppubsub.Inject(ctx, msg, slogcppubsub.WithPropagators(propagation.TraceContext{}))

	var buf bytes.Buffer
	h, err := slogcp.NewHandler(&buf, slogcp.WithTraceProjectID("proj-123"))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	t.Cleanup(func() {
		if cerr := h.Close(); cerr != nil {
			t.Errorf("handler close: %v", cerr)
		}
	})

	logger := slog.New(h)

	wrapped := slogcppubsub.WrapReceiveHandler(
		// handler verifies context metadata and emits a log entry.
		func(ctx context.Context, msg *pubsub.Message) {
			info, ok := slogcppubsub.InfoFromContext(ctx)
			if !ok || info == nil {
				t.Fatalf("expected MessageInfo in context")
			}
			if info.SubscriptionID != "orders-sub" {
				t.Fatalf("subscription id = %q, want %q", info.SubscriptionID, "orders-sub")
			}
			if info.TopicID != "orders" {
				t.Fatalf("topic id = %q, want %q", info.TopicID, "orders")
			}
			if info.MessageID != "msg-123" {
				t.Fatalf("message id = %q, want %q", info.MessageID, "msg-123")
			}

			slogcp.Logger(ctx).Info("handled")
		},
		slogcppubsub.WithLogger(logger),
		slogcppubsub.WithSubscriptionID("orders-sub"),
		slogcppubsub.WithTopicID("orders"),
		slogcppubsub.WithLogMessageID(true),
		slogcppubsub.WithLogOrderingKey(true),
		slogcppubsub.WithLogDeliveryAttempt(true),
		slogcppubsub.WithLogPublishTime(true),
		slogcppubsub.WithPropagators(propagation.TraceContext{}),
		slogcppubsub.WithProjectID("proj-123"),
		slogcppubsub.WithOTel(false),
	)

	wrapped(context.Background(), msg)

	entry := decodeLatestEntry(t, &buf)
	if got := entry["message"]; got != "handled" {
		t.Fatalf("message = %v, want %q", got, "handled")
	}
	if got := entry["messaging.system"]; got != "gcp_pubsub" {
		t.Fatalf("messaging.system = %v, want %q", got, "gcp_pubsub")
	}
	if got := entry["messaging.operation.name"]; got != "process" {
		t.Fatalf("messaging.operation.name = %v, want %q", got, "process")
	}
	if got := entry["messaging.destination.name"]; got != "orders-sub" {
		t.Fatalf("messaging.destination.name = %v, want %q", got, "orders-sub")
	}
	if got := entry["messaging.destination_publish.name"]; got != "orders" {
		t.Fatalf("messaging.destination_publish.name = %v, want %q", got, "orders")
	}
	if got := entry["messaging.message.id"]; got != "msg-123" {
		t.Fatalf("messaging.message.id = %v, want %q", got, "msg-123")
	}
	if got := entry["messaging.gcp_pubsub.message.ordering_key"]; got != "order-1" {
		t.Fatalf("messaging.gcp_pubsub.message.ordering_key = %v, want %q", got, "order-1")
	}

	if got, ok := entry["messaging.gcp_pubsub.message.delivery_attempt"].(json.Number); ok {
		if value, err := got.Int64(); err != nil || value != int64(attempt) {
			t.Fatalf("delivery_attempt = %v, want %d", got, attempt)
		}
	} else {
		t.Fatalf("expected delivery attempt number, got %T", entry["messaging.gcp_pubsub.message.delivery_attempt"])
	}

	if got, ok := entry["messaging.message.body.size"].(json.Number); ok {
		if value, err := got.Int64(); err != nil || value != int64(len(msg.Data)) {
			t.Fatalf("body size = %v, want %d", got, len(msg.Data))
		}
	} else {
		t.Fatalf("expected message body size number, got %T", entry["messaging.message.body.size"])
	}

	if got, ok := entry["pubsub.message.publish_time"].(string); ok {
		if got != publishTime.Format(time.RFC3339Nano) {
			t.Fatalf("publish_time = %q, want %q", got, publishTime.Format(time.RFC3339Nano))
		}
	} else {
		t.Fatalf("expected publish_time string, got %T", entry["pubsub.message.publish_time"])
	}

	traceValue, ok := entry[slogcp.TraceKey].(string)
	if !ok || !strings.Contains(traceValue, spanCtx.TraceID().String()) {
		t.Fatalf("trace field = %v, want trace id %s", entry[slogcp.TraceKey], spanCtx.TraceID().String())
	}
}

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

// decodeLatestEntry unmarshals the final JSON log line from buf.
func decodeLatestEntry(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()

	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	if len(lines) == 0 {
		t.Fatalf("expected log output")
	}

	dec := json.NewDecoder(bytes.NewReader(lines[len(lines)-1]))
	dec.UseNumber()

	var entry map[string]any
	if err := dec.Decode(&entry); err != nil {
		t.Fatalf("unmarshal log entry: %v", err)
	}
	return entry
}
