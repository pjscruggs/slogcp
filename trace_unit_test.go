//go:build unit
// +build unit

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

package slogcp_test

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/trace"

	"github.com/pjscruggs/slogcp/internal/gcp"
)

// mustTraceID converts a 32‑character hexadecimal string into a trace.TraceID.
//
// It calls t.Fatalf if the input is invalid, allowing callers to use literals
// without cluttering the test body with error handling.
func mustTraceID(t *testing.T, hexStr string) trace.TraceID {
	t.Helper()
	id, err := trace.TraceIDFromHex(hexStr)
	if err != nil {
		t.Fatalf("invalid TraceID hex %q: %v", hexStr, err)
	}
	return id
}

// mustSpanID converts a 16‑character hexadecimal string into a trace.SpanID.
//
// Like mustTraceID, it terminates the test with a helpful message if the input
// cannot be parsed.
func mustSpanID(t *testing.T, hexStr string) trace.SpanID {
	t.Helper()
	id, err := trace.SpanIDFromHex(hexStr)
	if err != nil {
		t.Fatalf("invalid SpanID hex %q: %v", hexStr, err)
	}
	return id
}

// TestExtractTraceSpan verifies that ExtractTraceSpan faithfully copies the
// OpenTelemetry identifiers, applies the “projects/{id}/traces/” prefix when a
// project ID is supplied, and correctly reports the sampling bit.
//
// The sub‑tests cover:
//
//  1. A sampled span with a project ID (happy‑path).
//  2. A sampled span with an empty project ID.
//  3. An unsampled span.
//  4. A context with no span.
//
// No external services, mocks, or protobuf builds are required, keeping this
// a fast “tier‑2” unit test.
func TestExtractTraceSpan(t *testing.T) {
	const (
		projectID      = "my-proj"
		rawTraceHex    = "70f5c2c7b3c0d8eead4837399ac5b327"
		rawSpanHex     = "5fa1c6de0d1e3e11"
		formattedTrace = "projects/my-proj/traces/" + rawTraceHex
	)

	validSampledSC := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    mustTraceID(t, rawTraceHex),
		SpanID:     mustSpanID(t, rawSpanHex),
		TraceFlags: trace.FlagsSampled,
	})

	validUnsampledSC := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: mustTraceID(t, rawTraceHex),
		SpanID:  mustSpanID(t, rawSpanHex),
	})

	tests := []struct {
		name           string
		ctx            context.Context
		projectID      string
		wantFmtTraceID string
		wantRawTraceID string
		wantRawSpanID  string
		wantSampled    bool
		wantValid      bool
	}{
		{
			name:           "sampled_with_project",
			ctx:            trace.ContextWithSpanContext(context.Background(), validSampledSC),
			projectID:      projectID,
			wantFmtTraceID: formattedTrace,
			wantRawTraceID: rawTraceHex,
			wantRawSpanID:  rawSpanHex,
			wantSampled:    true,
			wantValid:      true,
		},
		{
			name:           "sampled_no_project",
			ctx:            trace.ContextWithSpanContext(context.Background(), validSampledSC),
			projectID:      "",
			wantFmtTraceID: "",
			wantRawTraceID: rawTraceHex,
			wantRawSpanID:  rawSpanHex,
			wantSampled:    true,
			wantValid:      true,
		},
		{
			name:           "unsampled_span",
			ctx:            trace.ContextWithSpanContext(context.Background(), validUnsampledSC),
			projectID:      projectID,
			wantFmtTraceID: formattedTrace,
			wantRawTraceID: rawTraceHex,
			wantRawSpanID:  rawSpanHex,
			wantSampled:    false,
			wantValid:      true,
		},
		{
			name:           "no_span_in_context",
			ctx:            context.Background(),
			projectID:      projectID,
			wantFmtTraceID: "",
			wantRawTraceID: "",
			wantRawSpanID:  "",
			wantSampled:    false,
			wantValid:      false,
		},
	}

	for _, tc := range tests {
		tc := tc // capture loop variable
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			gotFmt, gotRawTrace, gotRawSpan, gotSampled, gotSC :=
				gcp.ExtractTraceSpan(tc.ctx, tc.projectID)

			if gotFmt != tc.wantFmtTraceID {
				t.Errorf("formattedTraceID = %q, want %q", gotFmt, tc.wantFmtTraceID)
			}
			if gotRawTrace != tc.wantRawTraceID {
				t.Errorf("rawTraceID = %q, want %q", gotRawTrace, tc.wantRawTraceID)
			}
			if gotRawSpan != tc.wantRawSpanID {
				t.Errorf("rawSpanID = %q, want %q", gotRawSpan, tc.wantRawSpanID)
			}
			if gotSampled != tc.wantSampled {
				t.Errorf("sampled = %v, want %v", gotSampled, tc.wantSampled)
			}

			if gotSC.IsValid() != tc.wantValid {
				t.Fatalf("SpanContext validity = %v, want %v", gotSC.IsValid(), tc.wantValid)
			}
			if tc.wantValid && !gotSC.Equal(validSampledSC) && !gotSC.Equal(validUnsampledSC) {
				t.Errorf("SpanContext mismatch: got %+v", gotSC)
			}
		})
	}
}
