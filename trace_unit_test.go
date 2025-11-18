package slogcp

import "testing"

// TestSpanIDHexToDecimalCoversSuccessAndFailure ensures hex conversion succeeds and fails appropriately.
func TestSpanIDHexToDecimalCoversSuccessAndFailure(t *testing.T) {
	t.Parallel()

	if dec, ok := SpanIDHexToDecimal("000000000000000a"); !ok || dec != "10" {
		t.Fatalf("SpanIDHexToDecimal success = (%q,%v), want (\"10\",true)", dec, ok)
	}
	if _, ok := SpanIDHexToDecimal("invalid-span"); ok {
		t.Fatalf("expected invalid span ID to fail conversion")
	}
}
