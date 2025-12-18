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

package slogcp

import (
	"context"
	"reflect"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
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

// TestEnsurePropagationInstallsCompositePropagator verifies EnsurePropagation
// replaces the global propagator when auto-set is enabled.
func TestEnsurePropagationInstallsCompositePropagator(t *testing.T) {
	t.Setenv("SLOGCP_PROPAGATOR_AUTOSET", "")

	stub := stubPropagator{}
	resetPropagatorForTest(t, stub)

	EnsurePropagation()
	if reflect.TypeOf(otel.GetTextMapPropagator()) == reflect.TypeOf(stub) {
		t.Fatalf("expected EnsurePropagation to replace stub propagator")
	}
}

// TestEnsurePropagationHonorsDisableFlag ensures the disable env var prevents mutation.
func TestEnsurePropagationHonorsDisableFlag(t *testing.T) {
	t.Setenv("SLOGCP_PROPAGATOR_AUTOSET", "false")

	stub := stubPropagator{}
	resetPropagatorForTest(t, stub)

	EnsurePropagation()
	if reflect.TypeOf(otel.GetTextMapPropagator()) != reflect.TypeOf(stub) {
		t.Fatalf("expected stub propagator to remain installed when auto-set disabled")
	}
}

// TestPropagatorAutoSetParsesValues exercises parsing of environment overrides without mutating the propagator.
func TestPropagatorAutoSetParsesValues(t *testing.T) {
	t.Setenv("SLOGCP_PROPAGATOR_AUTOSET", "TRUE")
	if !propagatorAutoSetEnabled() {
		t.Fatalf("propagatorAutoSetEnabled() = false, want true for TRUE")
	}

	t.Setenv("SLOGCP_PROPAGATOR_AUTOSET", "t")
	if !propagatorAutoSetEnabled() {
		t.Fatalf("propagatorAutoSetEnabled() = false, want true for t")
	}

	t.Setenv("SLOGCP_PROPAGATOR_AUTOSET", "1")
	if !propagatorAutoSetEnabled() {
		t.Fatalf("propagatorAutoSetEnabled() = false, want true for 1")
	}

	t.Setenv("SLOGCP_PROPAGATOR_AUTOSET", "false")
	if propagatorAutoSetEnabled() {
		t.Fatalf("propagatorAutoSetEnabled() = true, want false for false")
	}

	t.Setenv("SLOGCP_PROPAGATOR_AUTOSET", "F")
	if propagatorAutoSetEnabled() {
		t.Fatalf("propagatorAutoSetEnabled() = true, want false for F")
	}

	t.Setenv("SLOGCP_PROPAGATOR_AUTOSET", "0")
	if propagatorAutoSetEnabled() {
		t.Fatalf("propagatorAutoSetEnabled() = true, want false for 0")
	}

	t.Setenv("SLOGCP_PROPAGATOR_AUTOSET", "not-a-bool")
	if !propagatorAutoSetEnabled() {
		t.Fatalf("propagatorAutoSetEnabled() = false, want true for invalid values")
	}
}
