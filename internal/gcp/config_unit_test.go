//go:build unit
// +build unit

package gcp

import "testing"

func TestLoadConfigTraceProjectID(t *testing.T) {
	t.Setenv("SLOGCP_LOG_TARGET", "stdout")
	t.Setenv("SLOGCP_PROJECT_ID", "base")
	t.Setenv("SLOGCP_TRACE_PROJECT_ID", "trace")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() returned %v, want nil", err)
	}
	if cfg.ProjectID != "base" {
		t.Errorf("ProjectID = %q, want %q", cfg.ProjectID, "base")
	}
	if cfg.TraceProjectID != "trace" {
		t.Errorf("TraceProjectID = %q, want %q", cfg.TraceProjectID, "trace")
	}
}

func TestLoadConfigTraceProjectIDDefault(t *testing.T) {
	t.Setenv("SLOGCP_LOG_TARGET", "stdout")
	t.Setenv("SLOGCP_PROJECT_ID", "base")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() returned %v, want nil", err)
	}
	if cfg.TraceProjectID != "base" {
		t.Errorf("TraceProjectID = %q, want %q", cfg.TraceProjectID, "base")
	}
}
