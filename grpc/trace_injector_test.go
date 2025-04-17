package grpc

import (
	"encoding/binary"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/metadata"
)

// createMD is a helper to create metadata.MD with lowercase keys.
func createMD(key, value string) metadata.MD {
	return metadata.MD{
		strings.ToLower(key): []string{value},
	}
}

// TestParseTraceparent verifies parsing of the W3C traceparent header.
func TestParseTraceparent(t *testing.T) {
	validTraceIDHex := "4bf92f3577b34da6a3ce929d0e0e4736"
	validSpanIDHex := "00f067aa0ba902b7"
	validTraceID, _ := trace.TraceIDFromHex(validTraceIDHex)
	validSpanID, _ := trace.SpanIDFromHex(validSpanIDHex)

	// Expected contexts
	wantSampledCtx := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    validTraceID,
		SpanID:     validSpanID,
		TraceFlags: trace.FlagsSampled, // 01 means sampled
		Remote:     true,
	})
	wantNotSampledCtx := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    validTraceID,
		SpanID:     validSpanID,
		TraceFlags: 0, // 00 means not sampled
		Remote:     true,
	})

	testCases := []struct {
		name        string
		headerValue string
		wantContext trace.SpanContext // Expected context if wantOk is true
		wantOk      bool
	}{
		{"Valid sampled", fmt.Sprintf("00-%s-%s-01", validTraceIDHex, validSpanIDHex), wantSampledCtx, true},
		{"Valid not sampled", fmt.Sprintf("00-%s-%s-00", validTraceIDHex, validSpanIDHex), wantNotSampledCtx, true},
		{"Valid sampled with extra flags", fmt.Sprintf("00-%s-%s-03", validTraceIDHex, validSpanIDHex), wantSampledCtx, true}, // 03 = 00000011 -> sampled bit is set
		{"Invalid version", fmt.Sprintf("01-%s-%s-01", validTraceIDHex, validSpanIDHex), trace.SpanContext{}, false},
		{"Invalid number of parts", fmt.Sprintf("00-%s-%s", validTraceIDHex, validSpanIDHex), trace.SpanContext{}, false},
		{"Invalid trace ID hex", fmt.Sprintf("00-invalidtraceid-%s-01", validSpanIDHex), trace.SpanContext{}, false},
		{"Invalid span ID hex", fmt.Sprintf("00-%s-invalidspanid-01", validTraceIDHex), trace.SpanContext{}, false},
		{"Invalid flags hex", fmt.Sprintf("00-%s-%s-xx", validTraceIDHex, validSpanIDHex), trace.SpanContext{}, false},
		{"Zero trace ID", fmt.Sprintf("00-%s-%s-01", "00000000000000000000000000000000", validSpanIDHex), trace.SpanContext{}, false},
		{"Zero span ID", fmt.Sprintf("00-%s-%s-01", validTraceIDHex, "0000000000000000"), trace.SpanContext{}, false},
		{"Empty header value", "", trace.SpanContext{}, false},
		{"Too long trace ID", fmt.Sprintf("00-%s00-%s-01", validTraceIDHex, validSpanIDHex), trace.SpanContext{}, false},
		{"Too long span ID", fmt.Sprintf("00-%s-%s00-01", validTraceIDHex, validSpanIDHex), trace.SpanContext{}, false},
		{"Too long flags", fmt.Sprintf("00-%s-%s-0100", validTraceIDHex, validSpanIDHex), trace.SpanContext{}, false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md := createMD(traceparentKey, tc.headerValue)
			if tc.headerValue == "" {
				md = metadata.MD{} // Ensure key is absent for empty value test
			}

			gotContext, gotOk := parseTraceparent(md)

			if gotOk != tc.wantOk {
				t.Fatalf("parseTraceparent ok mismatch: got %v, want %v", gotOk, tc.wantOk)
			}

			if tc.wantOk {
				// Use cmp.Equal for robust SpanContext comparison.
				if diff := cmp.Diff(tc.wantContext, gotContext); diff != "" {
					t.Errorf("SpanContext mismatch (-want +got):\n%s", diff)
				}
				// Explicitly verify IsRemote() is true after parsing.
				if !gotContext.IsRemote() {
					t.Error("Expected IsRemote() to be true")
				}
			} else {
				// Verify context is invalid if parsing failed.
				if gotContext.IsValid() {
					t.Errorf("Expected invalid SpanContext when ok=false, but got valid: %v", gotContext)
				}
			}
		})
	}

	t.Run("HeaderNotPresent", func(t *testing.T) {
		// Test case where the header key is entirely absent.
		md := metadata.MD{}
		_, gotOk := parseTraceparent(md)
		if gotOk {
			t.Error("parseTraceparent returned ok=true when header was not present")
		}
	})
}

// TestParseXCloudTraceContext verifies parsing of the GCP X-Cloud-Trace-Context header.
func TestParseXCloudTraceContext(t *testing.T) {
	validTraceIDHex := "4bf92f3577b34da6a3ce929d0e0e4736"
	validSpanIDUint64 := uint64(1234567890123456) // Decimal Span ID
	validTraceID, _ := trace.TraceIDFromHex(validTraceIDHex)
	var validSpanID trace.SpanID
	binary.BigEndian.PutUint64(validSpanID[:], validSpanIDUint64) // Correct conversion

	// Expected contexts
	wantSampledCtx := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    validTraceID,
		SpanID:     validSpanID,
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	})
	wantNotSampledCtx := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    validTraceID,
		SpanID:     validSpanID,
		TraceFlags: 0, // Not sampled
		Remote:     true,
	})

	testCases := []struct {
		name        string
		headerValue string
		wantContext trace.SpanContext
		wantOk      bool
	}{
		{"Valid sampled (o=1)", fmt.Sprintf("%s/%d;o=1", validTraceIDHex, validSpanIDUint64), wantSampledCtx, true},
		{"Valid not sampled (o=0)", fmt.Sprintf("%s/%d;o=0", validTraceIDHex, validSpanIDUint64), wantNotSampledCtx, true},
		{"Valid no options", fmt.Sprintf("%s/%d", validTraceIDHex, validSpanIDUint64), wantNotSampledCtx, true},
		{"Valid with semicolon but no options", fmt.Sprintf("%s/%d;", validTraceIDHex, validSpanIDUint64), wantNotSampledCtx, true},
		{"Valid with other options ignored", fmt.Sprintf("%s/%d;o=1;other=ignored", validTraceIDHex, validSpanIDUint64), wantSampledCtx, true},
		{"Invalid format (no slash)", fmt.Sprintf("%s%d;o=1", validTraceIDHex, validSpanIDUint64), trace.SpanContext{}, false},
		{"Invalid trace ID hex", fmt.Sprintf("invalidtraceid/%d;o=1", validSpanIDUint64), trace.SpanContext{}, false},
		{"Invalid span ID decimal", fmt.Sprintf("%s/invalidspanid;o=1", validTraceIDHex), trace.SpanContext{}, false},
		{"Zero trace ID", fmt.Sprintf("00000000000000000000000000000000/%d;o=1", validSpanIDUint64), trace.SpanContext{}, false},
		{"Zero span ID", fmt.Sprintf("%s/0;o=1", validTraceIDHex), trace.SpanContext{}, false},
		{"Empty header value", "", trace.SpanContext{}, false},
		{"Only trace ID", validTraceIDHex, trace.SpanContext{}, false},
		{"Trace ID with slash but no span ID", validTraceIDHex + "/", trace.SpanContext{}, false},
		{"Trace ID with slash and semicolon", validTraceIDHex + "/;", trace.SpanContext{}, false}, // Span ID parse should fail
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Use the canonical lowercase key for creating the MD
			md := createMD(xCloudTraceContextKey, tc.headerValue)
			if tc.headerValue == "" {
				md = metadata.MD{}
			}

			gotContext, gotOk := parseXCloudTraceContext(md)

			if gotOk != tc.wantOk {
				t.Fatalf("parseXCloudTraceContext ok mismatch: got %v, want %v", gotOk, tc.wantOk)
			}

			if tc.wantOk {
				// Use cmp.Equal for robust SpanContext comparison.
				if diff := cmp.Diff(tc.wantContext, gotContext); diff != "" {
					t.Errorf("SpanContext mismatch (-want +got):\n%s", diff)
				}
				// Explicitly verify IsRemote() is true after parsing.
				if !gotContext.IsRemote() {
					t.Error("Expected IsRemote() to be true")
				}
			} else {
				// Verify context is invalid if parsing failed.
				if gotContext.IsValid() {
					t.Errorf("Expected invalid SpanContext when ok=false, but got valid: %v", gotContext)
				}
			}
		})
	}

	t.Run("HeaderNotPresent", func(t *testing.T) {
		// Test case where the header key is entirely absent.
		md := metadata.MD{}
		_, gotOk := parseXCloudTraceContext(md)
		if gotOk {
			t.Error("parseXCloudTraceContext returned ok=true when header was not present")
		}
	})
}
