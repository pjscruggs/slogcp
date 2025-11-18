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
	"errors"
	"io"
	"sync"
	"testing"
)

// resetRuntimeInfoCache clears cached runtime inspection state for isolated tests.
func resetRuntimeInfoCache() {
	runtimeInfoOnce = sync.Once{}
	runtimeInfo = RuntimeInfo{}
	resetHandlerConfigCache()
}

type stubMetadataClient struct {
	onGCE  bool
	values map[string]string
}

// OnGCE reports whether the stub considers metadata available.
func (s *stubMetadataClient) OnGCE() bool {
	return s.onGCE
}

// Get returns the stubbed metadata value or signals absence.
func (s *stubMetadataClient) Get(path string) (string, error) {
	if v, ok := s.values[path]; ok {
		return v, nil
	}
	return "", errors.New("metadata not found")
}

// withMetadataClient installs a temporary metadata client factory for the test scope.
func withMetadataClient(t *testing.T, client metadataClient) {
	original := metadataClientFactory
	metadataClientFactory = func() metadataClient {
		return client
	}
	t.Cleanup(func() {
		metadataClientFactory = original
	})
}

// TestDetectRuntimeInfoCloudRunService verifies Cloud Run environment variables populate runtime metadata.
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
	if info.Environment != RuntimeEnvCloudRunService {
		t.Fatalf("Environment = %v, want %v", info.Environment, RuntimeEnvCloudRunService)
	}
}

// TestDetectRuntimeInfoMetadataFallback ensures metadata server fallback supplies project ID.
func TestDetectRuntimeInfoMetadataFallback(t *testing.T) {
	resetRuntimeInfoCache()
	withMetadataClient(t, &stubMetadataClient{
		onGCE: true,
		values: map[string]string{
			"project/project-id": "meta-project",
		},
	})

	info := detectRuntimeInfo()
	if got := info.ProjectID; got != "meta-project" {
		t.Fatalf("ProjectID = %q, want %q", got, "meta-project")
	}
}

// TestNewHandlerUsesRuntimeProjectID confirms handler defaults trace project ID from runtime discovery.
func TestNewHandlerUsesRuntimeProjectID(t *testing.T) {
	resetRuntimeInfoCache()
	withMetadataClient(t, &stubMetadataClient{
		onGCE: true,
		values: map[string]string{
			"project/project-id": "meta-project",
		},
	})

	t.Setenv("SLOGCP_TRACE_PROJECT_ID", "")
	t.Setenv("SLOGCP_PROJECT_ID", "")
	t.Setenv("SLOGCP_GCP_PROJECT", "")
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
