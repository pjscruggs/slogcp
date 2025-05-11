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

package gcp

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

// contextWithSpan is a helper to create a context with a specific SpanContext for testing.
func contextWithSpan(traceID trace.TraceID, spanID trace.SpanID, flags trace.TraceFlags) context.Context {
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: flags,
		Remote:     true, // Assume remote context for extraction tests
	})
	// Use trace.ContextWithRemoteSpanContext to simulate context coming from upstream.
	return trace.ContextWithRemoteSpanContext(context.Background(), sc)
}

// TestExtractTraceSpan verifies extraction and formatting of trace context.
func TestExtractTraceSpan(t *testing.T) {
	projectID := "test-project-123"
	validTraceIDHex := "a1a2a3a4a5a6a7a8b1b2b3b4b5b6b7b8"
	validSpanIDHex := "c1c2c3c4c5c6c7c8"
	validTraceID, err := trace.TraceIDFromHex(validTraceIDHex)
	if err != nil {
		t.Fatalf("Setup error: failed to parse validTraceIDHex: %v", err)
	}
	validSpanID, err := trace.SpanIDFromHex(validSpanIDHex)
	if err != nil {
		t.Fatalf("Setup error: failed to parse validSpanIDHex: %v", err)
	}

	// Expected formatted trace ID string based on GCP requirements.
	wantFormattedTraceID := fmt.Sprintf("projects/%s/traces/%s", projectID, validTraceIDHex)

	testCases := []struct {
		name        string
		ctx         context.Context
		projectID   string
		wantTraceID string
		wantSpanID  string
		wantSampled bool
	}{
		{
			name:        "Valid context, sampled",
			ctx:         contextWithSpan(validTraceID, validSpanID, trace.FlagsSampled),
			projectID:   projectID,
			wantTraceID: wantFormattedTraceID,
			wantSpanID:  validSpanIDHex,
			wantSampled: true,
		},
		{
			name:        "Valid context, not sampled",
			ctx:         contextWithSpan(validTraceID, validSpanID, 0), // No flags set
			projectID:   projectID,
			wantTraceID: wantFormattedTraceID,
			wantSpanID:  validSpanIDHex,
			wantSampled: false,
		},
		{
			name:        "Background context (no span)",
			ctx:         context.Background(),
			projectID:   projectID,
			wantTraceID: "", // Expect empty strings and false when no span context exists
			wantSpanID:  "",
			wantSampled: false,
		},
		{
			name:        "Context with invalid span (zero traceID)",
			ctx:         contextWithSpan(trace.TraceID{}, validSpanID, trace.FlagsSampled),
			projectID:   projectID,
			wantTraceID: "", // SpanContext IsValid() returns false
			wantSpanID:  "",
			wantSampled: false,
		},
		{
			name:        "Context with invalid span (zero spanID)",
			ctx:         contextWithSpan(validTraceID, trace.SpanID{}, trace.FlagsSampled),
			projectID:   projectID,
			wantTraceID: "", // SpanContext IsValid() returns false
			wantSpanID:  "",
			wantSampled: false,
		},
		{
			name:        "Valid context, empty projectID",
			ctx:         contextWithSpan(validTraceID, validSpanID, trace.FlagsSampled),
			projectID:   "",    // Empty project ID prevents formatting
			wantTraceID: "",    // Expect empty as trace ID cannot be formatted
			wantSpanID:  "",    // Expect empty as trace ID is cleared
			wantSampled: false, // Expect false as trace ID is cleared
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			gotTraceID, _, gotSpanID, gotSampled, _ := ExtractTraceSpan(tc.ctx, tc.projectID)

			// Verify all three return values match expectations.
			if gotTraceID != tc.wantTraceID {
				t.Errorf("ExtractTraceSpan() traceID = %q, want %q", gotTraceID, tc.wantTraceID)
			}
			if gotSpanID != tc.wantSpanID {
				t.Errorf("ExtractTraceSpan() spanID = %q, want %q", gotSpanID, tc.wantSpanID)
			}
			if gotSampled != tc.wantSampled {
				t.Errorf("ExtractTraceSpan() sampled = %v, want %v", gotSampled, tc.wantSampled)
			}
		})
	}
}

// getTestPC returns the program counter of the test call site.
// It skips this helper plus any runtime.* and testing.* frames.
func getTestPC(t *testing.T) uintptr {
	t.Helper() // Mark this as a test helper
	pcs := make([]uintptr, 10)
	n := runtime.Callers(0, pcs)
	if n == 0 {
		t.Fatal("getTestPC: runtime.Callers returned no PCs")
	}
	frames := runtime.CallersFrames(pcs[:n])

	var foundHelper bool

	for {
		frame, more := frames.Next()
		fn := frame.Function

		// First identify this helper function
		if !foundHelper && strings.Contains(fn, "getTestPC") {
			foundHelper = true
			if !more {
				break
			}
			continue
		}

		// After finding helper, return the next non-runtime/testing frame
		if foundHelper && !strings.HasPrefix(fn, "runtime.") &&
			!strings.HasPrefix(fn, "testing.") {
			return frame.PC
		}

		if !more {
			break
		}
	}

	t.Fatal("getTestPC: Could not find a non-runtime/testing frame")
	return 0 // Should be unreachable
}

// TestResolveSourceLocation verifies conversion of PC to source file/line/function.
func TestResolveSourceLocation(t *testing.T) {
	t.Run("ZeroPC", func(t *testing.T) {
		// Verify zero PC returns nil, as no source info can be derived.
		got := resolveSourceLocation(0)
		if got != nil {
			t.Errorf("resolveSourceLocation(0) = %v, want nil", got)
		}
	})

	t.Run("ValidPC", func(t *testing.T) {
		pc := getTestPC(t) // PC of the line calling this in the test function
		if pc == 0 {
			t.Fatal("Could not get valid PC for test") // Fatal because setup failed
		}

		got := resolveSourceLocation(pc)
		if got == nil {
			t.Fatalf("resolveSourceLocation(validPC) returned nil, want populated struct")
		}

		// Perform robust checks focusing on suffixes and substrings for resilience.
		wantFileSuffix := "internal/gcp/trace_test.go"
		if !strings.HasSuffix(got.File, wantFileSuffix) {
			t.Errorf("SourceLocation.File = %q, want suffix %q", got.File, wantFileSuffix)
		}
		// Function name for t.Run's func is like ...TestResolveSourceLocation.funcN
		wantFuncSubstring := "TestResolveSourceLocation"
		if !strings.Contains(got.Function, wantFuncSubstring) {
			t.Errorf("SourceLocation.Function = %q, want substring %q", got.Function, wantFuncSubstring)
		}
		// Check line number is positive (basic sanity check). Exact line check is too fragile.
		if got.Line <= 0 {
			t.Errorf("SourceLocation.Line = %d, want positive value", got.Line)
		}
	})

	t.Run("InvalidPCLarge", func(t *testing.T) {
		// Verify large/invalid PC returns nil, as runtime functions likely fail.
		invalidPC := uintptr(1 << 63) // A likely invalid PC value
		got := resolveSourceLocation(invalidPC)
		if got != nil {
			t.Errorf("resolveSourceLocation(invalidPC) = %v, want nil", got)
		}
	})
}
