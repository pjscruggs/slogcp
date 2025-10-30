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

//go:build unit
// +build unit

package grpc

import (
	"context"
	"fmt"
	"strconv"
	"testing"

	gcppropagator "github.com/GoogleCloudPlatform/opentelemetry-operations-go/propagator"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/metadata"
)

// TestCompositePropagatorExtractsMetadata ensures that the configured composite propagator
// can extract both W3C traceparent and Google X-Cloud-Trace-Context metadata.
func TestCompositePropagatorExtractsMetadata(t *testing.T) {
	traceHex := "70f5c2c7b3c0d8eead4837399ac5b327"
	spanHex := "5fa1c6de0d1e3e11"
	spanUint, err := strconv.ParseUint(spanHex, 16, 64)
	if err != nil {
		t.Fatalf("ParseUint(%q) returned %v", spanHex, err)
	}
	spanDec := strconv.FormatUint(spanUint, 10)

	propagator := propagation.NewCompositeTextMapPropagator(
		gcppropagator.CloudTraceOneWayPropagator{},
		propagation.TraceContext{},
		propagation.Baggage{},
	)
	otel.SetTextMapPropagator(propagator)

	metaCarrier := func(m map[string]string) metadataCarrier {
		return metadataCarrier{md: metadata.New(m)}
	}

	// W3C traceparent extraction
	ctx := otel.GetTextMapPropagator().Extract(context.Background(), metaCarrier(map[string]string{
		"traceparent": fmt.Sprintf("00-%s-%s-01", traceHex, spanHex),
	}))
	if sc := trace.SpanContextFromContext(ctx); !sc.IsValid() || sc.TraceID().String() != traceHex || sc.SpanID().String() != spanHex || !sc.IsSampled() {
		t.Fatalf("unexpected span context from traceparent: %v", sc)
	}

	// X-Cloud-Trace-Context extraction
	ctx = otel.GetTextMapPropagator().Extract(context.Background(), metaCarrier(map[string]string{
		"x-cloud-trace-context": fmt.Sprintf("%s/%s;o=1", traceHex, spanDec),
	}))
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		t.Fatalf("span context invalid after X-Cloud extraction")
	}
	if sc.TraceID().String() != traceHex || sc.SpanID().String() != spanHex || !sc.IsSampled() {
		t.Fatalf("unexpected span context from X-Cloud-Trace-Context: %v", sc)
	}
}

// TestFormatXCloudTraceContextFromSpanContext checks formatting back to header form from SpanContexts.
func TestFormatXCloudTraceContextFromSpanContext(t *testing.T) {
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    mustTraceID(t, "70f5c2c7b3c0d8eead4837399ac5b327"),
		SpanID:     mustSpanID(t, "5fa1c6de0d1e3e11"),
		TraceFlags: trace.FlagsSampled,
	})
	want := "70f5c2c7b3c0d8eead4837399ac5b327/6891007561858694673;o=1"
	if got := formatXCloudTraceContextFromSpanContext(sc); got != want {
		t.Errorf("formatXCloudTraceContextFromSpanContext() = %q, want %q", got, want)
	}
	if got := formatXCloudTraceContextFromSpanContext(trace.SpanContext{}); got != "" {
		t.Errorf("formatXCloudTraceContextFromSpanContext(invalid) = %q, want empty", got)
	}
}

// mustTraceID returns a TraceID parsed from hex or fails the test immediately.
func mustTraceID(t *testing.T, hex string) trace.TraceID {
	t.Helper()
	id, err := trace.TraceIDFromHex(hex)
	if err != nil {
		t.Fatalf("TraceIDFromHex(%q) returned %v", hex, err)
	}
	return id
}

// mustSpanID returns a SpanID parsed from hex or fails the test immediately.
func mustSpanID(t *testing.T, hex string) trace.SpanID {
	t.Helper()
	id, err := trace.SpanIDFromHex(hex)
	if err != nil {
		t.Fatalf("SpanIDFromHex(%q) returned %v", hex, err)
	}
	return id
}
