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

package slogcp

import (
	"context"
	"reflect"
	"slices"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// stubPropagator implements propagation.TextMapPropagator for testing toggles.
type stubPropagator struct{}

// Inject satisfies propagation.TextMapPropagator for test doubles.
func (stubPropagator) Inject(context.Context, propagation.TextMapCarrier) {}

// Extract satisfies propagation.TextMapPropagator and returns the supplied context.
func (stubPropagator) Extract(ctx context.Context, _ propagation.TextMapCarrier) context.Context {
	return ctx
}

// Fields reports the headers influenced by the stub propagator.
func (stubPropagator) Fields() []string { return nil }

// resetPropagatorForTest swaps the global propagator and resets the once guard.
func resetPropagatorForTest(tb testing.TB, prop propagation.TextMapPropagator) {
	tb.Helper()
	otel.SetTextMapPropagator(prop)
	installPropagatorOnce = sync.Once{}
}

// TestNewCompositePropagatorIncludesW3CAndBaggage verifies the helper builds a usable propagator.
func TestNewCompositePropagatorIncludesW3CAndBaggage(t *testing.T) {
	p := NewCompositePropagator()

	fields := p.Fields()
	has := func(key string) bool {
		return slices.Contains(fields, key)
	}
	if !has("traceparent") {
		t.Fatalf("propagator fields missing traceparent: %v", fields)
	}
	if !has("tracestate") {
		t.Fatalf("propagator fields missing tracestate: %v", fields)
	}
	if !has("baggage") {
		t.Fatalf("propagator fields missing baggage: %v", fields)
	}
}

// TestCompositePropagatorZeroValueIsNoOp verifies the zero value behaves safely.
func TestCompositePropagatorZeroValueIsNoOp(t *testing.T) {
	var p CompositePropagator
	carrier := propagation.MapCarrier{}
	ctx := context.Background()

	p.Inject(ctx, carrier)
	if got := carrier.Get("traceparent"); got != "" {
		t.Fatalf("zero-value propagator injected traceparent: %q", got)
	}

	extracted := p.Extract(ctx, carrier)
	if extracted != ctx {
		t.Fatalf("zero-value propagator should return original context")
	}

	if fields := p.Fields(); fields != nil {
		t.Fatalf("zero-value propagator fields = %v, want nil", fields)
	}
}

// TestCompositePropagatorInjectExtract verifies non-zero behavior for trace context.
func TestCompositePropagatorInjectExtract(t *testing.T) {
	p := NewCompositePropagator()
	traceID, _ := trace.TraceIDFromHex("105445aa7843bc8bf206b12000100000")
	spanID, _ := trace.SpanIDFromHex("09158d8185d3c3af")
	spanCtx := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), spanCtx)
	carrier := propagation.MapCarrier{}

	p.Inject(ctx, carrier)
	if got := carrier.Get("traceparent"); got == "" {
		t.Fatalf("inject did not set traceparent")
	}

	extractedCtx := p.Extract(context.Background(), carrier)
	extractedSC := trace.SpanContextFromContext(extractedCtx)
	if !extractedSC.IsValid() {
		t.Fatalf("extract did not produce a valid span context")
	}
	if extractedSC.TraceID() != traceID {
		t.Fatalf("extracted trace id = %s, want %s", extractedSC.TraceID(), traceID)
	}
}

// TestEnsurePropagationInstallsCompositePropagator verifies EnsurePropagation replaces the global propagator.
func TestEnsurePropagationInstallsCompositePropagator(t *testing.T) {
	stub := stubPropagator{}
	resetPropagatorForTest(t, stub)

	EnsurePropagation()
	if reflect.TypeOf(otel.GetTextMapPropagator()) == reflect.TypeFor[stubPropagator]() {
		t.Fatalf("expected EnsurePropagation to replace stub propagator")
	}
}

// TestEnsurePropagationDoesNotOverrideAfterFirstInstall verifies once semantics.
func TestEnsurePropagationDoesNotOverrideAfterFirstInstall(t *testing.T) {
	stub := stubPropagator{}
	resetPropagatorForTest(t, stub)

	EnsurePropagation()
	if reflect.TypeOf(otel.GetTextMapPropagator()) == reflect.TypeFor[stubPropagator]() {
		t.Fatalf("expected first EnsurePropagation call to replace stub propagator")
	}

	// Simulate an application/library overriding the global propagator after install.
	otel.SetTextMapPropagator(stub)
	EnsurePropagation()
	if reflect.TypeOf(otel.GetTextMapPropagator()) != reflect.TypeFor[stubPropagator]() {
		t.Fatalf("EnsurePropagation should not override global propagator after first install")
	}
}
