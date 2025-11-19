package slogcp

import (
	"sync"
	"testing"
)

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

// TestDetectTraceProjectIDFromEnvPriority ensures higher-priority variables win.
func TestDetectTraceProjectIDFromEnvPriority(t *testing.T) {
	t.Parallel()

	t.Setenv("SLOGCP_PROJECT_ID", "project-id")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "gcp-project")
	t.Setenv("SLOGCP_TRACE_PROJECT_ID", "trace-id")

	if got := detectTraceProjectIDFromEnv(); got != "trace-id" {
		t.Fatalf("detectTraceProjectIDFromEnv() = %q, want %q", got, "trace-id")
	}
}

// TestCachedTraceProjectIDCachesFirstValue verifies the cached helper does not reread env vars.
func TestCachedTraceProjectIDCachesFirstValue(t *testing.T) {
	t.Parallel()

	resetTraceProjectEnvCache()
	t.Cleanup(resetTraceProjectEnvCache)

	t.Setenv("SLOGCP_TRACE_PROJECT_ID", "initial-project")
	if got := cachedTraceProjectID(); got != "initial-project" {
		t.Fatalf("cachedTraceProjectID() = %q, want %q", got, "initial-project")
	}

	t.Setenv("SLOGCP_TRACE_PROJECT_ID", "mutated")
	if got := cachedTraceProjectID(); got != "initial-project" {
		t.Fatalf("cachedTraceProjectID() after env change = %q, want cached %q", got, "initial-project")
	}
}

// resetTraceProjectEnvCache clears the cached project ID for trace tests.
func resetTraceProjectEnvCache() {
	traceProjectEnvOnce = sync.Once{}
	traceProjectEnvID = ""
}
