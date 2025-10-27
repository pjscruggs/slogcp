package slogcp

import (
	"io"
	"sync"
	"testing"
)

func resetRuntimeInfoCache() {
	runtimeInfoOnce = sync.Once{}
	runtimeInfo = RuntimeInfo{}
}

func TestDetectRuntimeInfoCloudRunService(t *testing.T) {
	resetRuntimeInfoCache()
	t.Setenv("K_SERVICE", "svc")
	t.Setenv("K_REVISION", "rev")
	t.Setenv("K_CONFIGURATION", "cfg")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "my-project")

	info := detectRuntimeInfo()
	if got := info.ProjectID; got != "my-project" {
		t.Fatalf("ProjectID = %q, want %q", got, "my-project")
	}
	if info.ServiceContext["service"] != "svc" {
		t.Fatalf("service context service = %q, want %q", info.ServiceContext["service"], "svc")
	}
	if info.ServiceContext["version"] != "rev" {
		t.Fatalf("service context version = %q, want %q", info.ServiceContext["version"], "rev")
	}
	if info.Labels["cloud_run.service"] != "svc" {
		t.Fatalf("label cloud_run.service = %q, want %q", info.Labels["cloud_run.service"], "svc")
	}
}

func TestDetectRuntimeInfoMetadataFallback(t *testing.T) {
	resetRuntimeInfoCache()
	originalFetch := metadataFetch
	metadataFetch = func(path string) (string, bool) {
		switch path {
		case "project/project-id":
			return "meta-project", true
		default:
			return "", false
		}
	}
	t.Cleanup(func() {
		metadataFetch = originalFetch
	})

	info := detectRuntimeInfo()
	if got := info.ProjectID; got != "meta-project" {
		t.Fatalf("ProjectID = %q, want %q", got, "meta-project")
	}
}

func TestNewHandlerUsesRuntimeProjectID(t *testing.T) {
	resetRuntimeInfoCache()
	originalFetch := metadataFetch
	metadataFetch = func(path string) (string, bool) {
		switch path {
		case "project/project-id":
			return "meta-project", true
		default:
			return "", false
		}
	}
	t.Cleanup(func() {
		metadataFetch = originalFetch
	})

	t.Setenv("SLOGCP_TRACE_PROJECT_ID", "")
	t.Setenv("SLOGCP_PROJECT_ID", "")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")
	t.Setenv("GCLOUD_PROJECT", "")
	t.Setenv("GCP_PROJECT", "")
	t.Setenv("PROJECT_ID", "")

	h, err := NewHandler(io.Discard)
	if err != nil {
		t.Fatalf("NewHandler returned error: %v", err)
	}
	if got := h.cfg.TraceProjectID; got != "meta-project" {
		t.Fatalf("TraceProjectID = %q, want %q", got, "meta-project")
	}
}
