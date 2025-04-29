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
	validTraceID, _ := trace.TraceIDFromHex(validTraceIDHex)
	validSpanID, _ := trace.SpanIDFromHex(validSpanIDHex)

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
			gotTraceID, gotSpanID, gotSampled := ExtractTraceSpan(tc.ctx, tc.projectID)

			// Verify all three return values match expectations.
			if gotTraceID != tc.wantTraceID {
				t.Errorf("ExtractTraceSpan traceID mismatch: got %q, want %q", gotTraceID, tc.wantTraceID)
			}
			if gotSpanID != tc.wantSpanID {
				t.Errorf("ExtractTraceSpan spanID mismatch: got %q, want %q", gotSpanID, tc.wantSpanID)
			}
			if gotSampled != tc.wantSampled {
				t.Errorf("ExtractTraceSpan sampled mismatch: got %v, want %v", gotSampled, tc.wantSampled)
			}
		})
	}
}

// getTestPC returns the program counter of the test call site.
// It skips this helper plus any runtime.* and testing.* frames.
func getTestPC() uintptr {
	pcs := make([]uintptr, 10)
	n := runtime.Callers(2, pcs) // skip runtime.Callers + this function
	frames := runtime.CallersFrames(pcs[:n])

	for {
		frame, more := frames.Next()
		fn := frame.Function

		// Skip this helper, runtime and testing frames.
		if strings.Contains(fn, "internal/gcp.getTestPC") ||
			strings.HasPrefix(fn, "runtime.") ||
			strings.HasPrefix(fn, "testing.") {
			if !more {
				break
			}
			continue
		}

		return frame.PC
	}

	return 0
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

	t.Run("ValidPC_Direct", func(t *testing.T) {
		pc := getTestPC() // PC of this line in the test function
		if pc == 0 {
			t.Skip("Could not get valid PC for test") // Should not happen normally
		}

		got := resolveSourceLocation(pc)
		if got == nil {
			t.Fatalf("resolveSourceLocation(validPC) returned nil, want populated struct")
		}

		// Perform robust checks focusing on suffixes and substrings for resilience.
		wantFileSuffix := "internal/gcp/trace_test.go"
		if !strings.HasSuffix(got.File, wantFileSuffix) {
			t.Errorf("SourceLocation.File mismatch: got %q, want suffix %q", got.File, wantFileSuffix)
		}
		// Function name for t.Run's func is like ...TestResolveSourceLocation.funcN
		wantFuncSubstring := "TestResolveSourceLocation"
		if !strings.Contains(got.Function, wantFuncSubstring) {
			t.Errorf("SourceLocation.Function mismatch: got %q, want substring %q", got.Function, wantFuncSubstring)
		}
		// Check line number is positive (basic sanity check). Exact line check is too fragile.
		if got.Line <= 0 {
			t.Errorf("SourceLocation.Line expected positive, got %d", got.Line)
		}
	})

	t.Run("ValidPC_NestedFunc", func(t *testing.T) {
		pc := getTestPC() // PC of the line calling this in the test function
		if pc == 0 {
			t.Skip("Could not get valid PC for test")
		}

		got := resolveSourceLocation(pc)
		if got == nil {
			t.Fatalf("resolveSourceLocation(validPC) returned nil, want populated struct")
		}

		// Perform robust checks similar to the direct case.
		wantFileSuffix := "internal/gcp/trace_test.go"
		if !strings.HasSuffix(got.File, wantFileSuffix) {
			t.Errorf("SourceLocation.File mismatch: got %q, want suffix %q", got.File, wantFileSuffix)
		}
		// Function name should be the *caller* of getTestPC, which is the t.Run func.
		wantFuncSubstring := "TestResolveSourceLocation"
		if !strings.Contains(got.Function, wantFuncSubstring) {
			t.Errorf("SourceLocation.Function mismatch: got %q, want substring %q", got.Function, wantFuncSubstring)
		}
		// Check line number is positive.
		if got.Line <= 0 {
			t.Errorf("SourceLocation.Line expected positive, got %d", got.Line)
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
